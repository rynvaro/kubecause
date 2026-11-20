package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/store"
)

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
		reply.WriteString(fmt.Sprintf("\n初步判断是容器内存限制过低，建议把 %s 容器的内存 limit 从 %s 调到 %s。这只是一张建议卡，不会自动执行——如果你想走正式流程，可以点“创建可审批的修复任务”，生成提案后人工审批再执行。", sr.Container, sr.OldValue, sr.NewValue))
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
