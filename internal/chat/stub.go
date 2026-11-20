package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// imageTokenRe 从用户消息里提取一个镜像名（带 tag），
// 例如“换成 nginx:1.25”里的 nginx:1.25。
var imageTokenRe = regexp.MustCompile(`([a-z0-9][a-z0-9._/-]*:[a-zA-Z0-9._-]+)`)

// extractImageChange 判断用户这轮是不是在说“把镜像换成 xxx”：
// 消息里要有一个镜像名，并且是换/改/升级的意思，
// 或者上一轮模型刚问过“换成哪个镜像”，用户这轮只回了一个镜像名。
// 多轮追问的记忆靠会话历史：上一轮的提问在 history 里，
// 这轮只回“nginx:1.25”也能接上，不用用户把整句话重说一遍。
func extractImageChange(text string, history []store.ConversationMessage) string {
	m := imageTokenRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	if strings.Contains(text, "换") || strings.Contains(text, "改") || strings.Contains(text, "升级") {
		return m[1]
	}
	for _, h := range history {
		if h.Role == "assistant" && (strings.Contains(h.Content, "哪个镜像") || strings.Contains(h.Content, "换成哪个")) {
			return m[1]
		}
	}
	return ""
}

// containerImageFromData 从 get_deployment 的结构化结果里读第一个容器的名字和镜像。
func containerImageFromData(data any) (string, string) {
	if d, ok := data.(map[string]any); ok {
		if containers, ok := d["containers"].([]map[string]string); ok && len(containers) > 0 {
			return containers[0]["name"], containers[0]["image"]
		}
	}
	return "", ""
}

// deploymentMentionedInText 看用户消息里是否提到了某个已知的 Deployment 名。
func deploymentMentionedInText(ctx context.Context, s *Service, text, namespace string) string {
	if out, err := s.Tools.ListDeployments(ctx, namespace); err == nil {
		b, _ := json.Marshal(out.Data)
		var v struct {
			Deployments []struct {
				Name string `json:"name"`
			} `json:"deployments"`
		}
		if err := json.Unmarshal(b, &v); err == nil {
			for _, d := range v.Deployments {
				if d.Name != "" && strings.Contains(text, d.Name) {
					return d.Name
				}
			}
		}
	}
	return ""
}

// runStub 是 Stub 模型的对话路径：确定性规则，不调真模型。
// 规则按用户意图分三档，顺序不能乱：
//  1. 问“当前状态 / 总览”：先 get_cluster_overview，再 list_problem_pods；
//  2. 问某个 namespace / 某个 Pod 为什么有问题：先解析 namespace
//     （消息里明确提到的优先，其次是会话历史里最近提到的），
//     再 list_problem_pods 找问题 Pod，然后对第一个问题 Pod
//     依次查 get_pod、get_events、get_pod_logs、get_deployment、search_runbooks；
//  3. 其他问题：先看全貌，再如实说查到了什么、还需要什么信息。
//
// 每一步工具调用都记进 ToolCalls 并以 tool 角色落盘，
// 最后的助手回复带结构化结果和可选的修复建议卡。
func (s *Service) runStub(ctx context.Context, conversationID, text string, history []store.ConversationMessage) (*Reply, error) {
	lower := strings.ToLower(text)
	var recs []ToolCallRecord
	var outputs []agent.ToolOutput
	structured := map[string]any{}
	var reply strings.Builder

	call := func(name string, args map[string]string) (agent.ToolOutput, error) {
		if args == nil {
			args = map[string]string{}
		}
		tc := &agent.ToolCall{ID: fmt.Sprintf("chat-call-%d", len(recs)+1), Name: name, Args: args}
		start := time.Now()
		out, err := s.executeChatTool(ctx, tc)
		rec := ToolCallRecord{ID: tc.ID, Name: name, Args: args, DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			// 工具失败也要记下来并回传给下一轮规则：
			// 规则层看到失败会换个工具重试，而不是原地重复调同一个。
			rec.Error = err.Error()
			rec.Summary = "工具执行失败：" + err.Error()
			recs = append(recs, rec)
			s.recordTool(ctx, conversationID, rec, agent.ToolOutput{Summary: rec.Summary})
			return agent.ToolOutput{}, err
		}
		rec.Summary = out.Summary
		recs = append(recs, rec)
		outputs = append(outputs, out)
		s.recordTool(ctx, conversationID, rec, out)
		return out, nil
	}

	isOverview := strings.Contains(text, "状态") || strings.Contains(text, "总览") ||
		strings.Contains(lower, "overview") || strings.Contains(text, "怎么样") ||
		strings.Contains(text, "如何")
	ns := s.resolveNamespace(ctx, text, history)

	// 主动变更 / 多轮追问选镜像（Slice 9）：用户明确说了要换成哪个镜像，
	// 不用先做故障诊断，直接查当前镜像、给一张 Patch 建议卡。
	// Pod 是健康的也不用先找故障：主动变更和故障修复走同一套 Patch 流程。
	// 触发条件在 extractImageChange 里：消息里有镜像名 + 换/改/升级，
	// 或者上一轮模型刚问过选哪个镜像、这轮只回了一个镜像名。
	if newImage := extractImageChange(text, history); newImage != "" {
		if ns == "" {
			reply.WriteString("你想换哪个命名空间的镜像？请告诉我 namespace，例如 demo。")
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		deployment := deploymentMentionedInText(ctx, s, text, ns)
		if deployment == "" {
			_, deployment = resolveTargetFromHistory(ctx, s, history, ns)
		}
		if deployment == "" {
			reply.WriteString("你想换哪个 Deployment 的镜像？请告诉我 Deployment 的名字，例如 demo-api。")
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		depOut, err := call("get_deployment", map[string]string{"namespace": ns, "deployment": deployment})
		if err != nil {
			return &Reply{Reply: "查当前镜像时出错：" + err.Error(), ToolCalls: recs, Structured: structured}, nil
		}
		structured["deployment"] = depOut.Data
		container, currentImage := containerImageFromData(depOut.Data)
		if container == "" {
			reply.WriteString(fmt.Sprintf("查不到 %s/%s 的容器，没法确认当前镜像。", ns, deployment))
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		if currentImage == newImage {
			reply.WriteString(fmt.Sprintf("%s/%s 容器 %s 的镜像已经是 %s，不用换。", ns, deployment, container, newImage))
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		sr := &agent.SuggestedRepair{
			Namespace: ns, Deployment: deployment, Container: container,
			Field: "image", OldValue: currentImage, NewValue: newImage,
			Reason: "用户要求把镜像换成 " + newImage,
		}
		sr.Normalize()
		if err := repair.ValidatePatch(sr.Namespace, sr.Deployment, sr.Patches); err != nil {
			reply.WriteString("这个镜像换不了：" + err.Error())
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		structured["suggestedRepair"] = sr
		reply.WriteString(fmt.Sprintf("查到了：%s/%s 容器 %s 当前镜像是 %s，建议换成 %s。这只是一张建议卡，不会自动执行——如果你想走正式流程，可以点“创建可审批的修复任务”，生成提案后人工审批再执行。", ns, deployment, container, currentImage, newImage))
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured, SuggestedRepair: sr}, nil
	}

	if isOverview {
		ov, err := call("get_cluster_overview", map[string]string{})
		if err == nil {
			structured["overview"] = ov.Data
			reply.WriteString(ov.Summary + "。")
		}
		pp, err := call("list_problem_pods", map[string]string{"namespace": ""})
		if err == nil {
			structured["problemPods"] = pp.Data
			reply.WriteString("\n" + pp.Summary + "。")
			if countOf(pp.Data) > 0 {
				reply.WriteString("你可以继续问某个命名空间或某个 Pod 是怎么回事，我会接着查。")
			} else {
				reply.WriteString("目前没有发现有问题的 Pod。")
			}
		}
		if reply.Len() == 0 {
			reply.WriteString("暂时查不到集群状态，请确认集群连接是否正常。")
		}
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
	}

	// 问具体问题：先定 namespace，定不下来就主动问，不猜。
	if ns == "" {
		reply.WriteString("你想查哪个命名空间的问题？请告诉我 namespace，例如 demo，或者先问一句“当前状态怎么样”，我先把有问题的 Pod 列出来。")
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
	}
	pp, err := call("list_problem_pods", map[string]string{"namespace": ns})
	if err != nil {
		return &Reply{Reply: "查问题 Pod 时出错：" + err.Error(), ToolCalls: recs, Structured: structured}, nil
	}
	structured["problemPods"] = pp.Data
	if countOf(pp.Data) == 0 {
		reply.WriteString(fmt.Sprintf("%s 命名空间下目前没有发现有问题的 Pod。", ns))
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
	}
	// 取第一个问题 Pod 深挖：状态、事件、日志、Deployment、手册。
	podName, deployment := firstProblemPod(pp.Data)
	if podName == "" {
		reply.WriteString(pp.Summary + "，但没能定位到具体的 Pod，请告诉我 Pod 名字。")
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
	}
	if deployment == "" {
		deployment = deploymentFromText(text, pp.Data)
	}
	if pod, err := call("get_pod", map[string]string{"namespace": ns, "pod": podName}); err == nil {
		structured["pod"] = pod.Data
		reply.WriteString(pod.Summary + "。")
	}
	if ev, err := call("get_events", map[string]string{"namespace": ns, "name": podName}); err == nil {
		structured["events"] = ev.Data
		reply.WriteString("\n" + ev.Summary + "。")
	}
	// 先查 Deployment 再读日志：日志要容器名，容器名从 Deployment 里读，
	// 不要写死容器名。
	if deployment != "" {
		if dep, err := call("get_deployment", map[string]string{"namespace": ns, "deployment": deployment}); err == nil {
			structured["deployment"] = dep.Data
			reply.WriteString("\n" + dep.Summary + "。")
		}
	}
	container := containerFromOutputs(outputs)
	if logs, err := call("get_pod_logs", map[string]string{"namespace": ns, "pod": podName, "container": container, "previous": "false"}); err == nil {
		structured["logs"] = logs.Data
		reply.WriteString("\n" + logs.Summary + "。")
	}
	keyword := keywordFromOutputs(outputs)
	if rb, err := call("search_runbooks", map[string]string{"query": keyword}); err == nil {
		structured["runbooks"] = rb.Data
		reply.WriteString("\n" + rb.Summary + "。")
	}
	// 修复建议卡：证据指向 OOM 时才给，只是一张卡，不执行、不生成提案。
	// 注意：这里调 synthesizeRepair 是桩模型测试路径的确定性规则，
	// 真模型路径不走这条路（见 real.go）。建议卡同时写进 Structured，
	// 这样 RunMessage 落盘后刷新会话也不会丢（和 real 路径一致）。
	var sr *agent.SuggestedRepair
	if deployment != "" {
		sr = synthesizeRepair(ctx, s, outputs, ns, deployment)
	}
	if sr != nil {
		structured["suggestedRepair"] = sr
		if len(sr.Patches) > 0 && strings.Contains(sr.Patches[0].Path, "/image") {
			reply.WriteString(fmt.Sprintf("\n初步判断是镜像 tag 不存在，建议把 %s 容器的镜像从 %s 换成 %s。这只是一张建议卡，不会自动执行——如果你想走正式流程，可以点“创建可审批的修复任务”，生成提案后人工审批再执行。", sr.Container, sr.OldValue, sr.NewValue))
		} else {
			reply.WriteString(fmt.Sprintf("\n初步判断是容器内存限制过低，建议把 %s 容器的内存 limit 从 %s 调到 %s。这只是一张建议卡，不会自动执行——如果你想走正式流程，可以点“创建可审批的修复任务”，生成提案后人工审批再执行。", sr.Container, sr.OldValue, sr.NewValue))
		}
	} else {
		reply.WriteString("\n目前没有明确的修复建议，需要更多证据才能下结论。")
	}
	return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured, SuggestedRepair: sr}, nil
}

// countOf 从工具返回的 Data 里读 count 字段。
func countOf(data any) int {
	b, _ := json.Marshal(data)
	var v struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0
	}
	return v.Count
}

// firstProblemPod 从 list_problem_pods 的结构化结果里取第一个问题 Pod。
func firstProblemPod(data any) (podName, deployment string) {
	b, _ := json.Marshal(data)
	var v struct {
		Pods []struct {
			Name       string `json:"name"`
			Deployment string `json:"deployment"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(b, &v); err != nil || len(v.Pods) == 0 {
		return "", ""
	}
	return v.Pods[0].Name, v.Pods[0].Deployment
}

// deploymentFromText 看用户消息里是否提到了某个 Deployment 名。
func deploymentFromText(text string, data any) string {
	b, _ := json.Marshal(data)
	var v struct {
		Pods []struct {
			Deployment string `json:"deployment"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ""
	}
	for _, p := range v.Pods {
		if p.Deployment != "" && strings.Contains(text, p.Deployment) {
			return p.Deployment
		}
	}
	return ""
}

// containerFromOutputs 从已收集的工具结果里找容器名，
// get_deployment 的结果里有 containers 列表，取第一个。
// 读不到时返回 fake 固定数据的容器名兜底（Stub 只在 fake/stub 回归里用），
// 真集群上容器名一定来自上一步 get_deployment 的结果。
func containerFromOutputs(outputs []agent.ToolOutput) string {
	for _, out := range outputs {
		if data, ok := out.Data.(map[string]any); ok {
			if containers, ok := data["containers"].([]map[string]string); ok && len(containers) > 0 {
				return containers[0]["name"]
			}
		}
	}
	return "demo-api"
}

// keywordFromOutputs 按已收集的证据选检索关键词，
// 一手证据优先，检索结果不压过 Pod / Events / 日志。
func keywordFromOutputs(outputs []agent.ToolOutput) string {
	joined := ""
	for _, out := range outputs {
		joined += out.Summary + " "
		b, _ := json.Marshal(out.Data)
		joined += string(b) + " "
	}
	switch {
	case strings.Contains(joined, "OOMKilled") || strings.Contains(joined, "out of memory"):
		return "OOMKilled"
	case strings.Contains(joined, "ImagePullBackOff") || strings.Contains(joined, "Failed to pull image"):
		return "ImagePullBackOff"
	case strings.Contains(joined, "Liveness probe failed") || strings.Contains(joined, "Unhealthy"):
		return "Liveness probe failed"
	default:
		return "CrashLoopBackOff"
	}
}
