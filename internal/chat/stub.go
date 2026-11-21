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
func (s *Service) runStub(ctx context.Context, conversationID, roundID, text string, history []store.ConversationMessage) (*Reply, error) {
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
			s.recordTool(ctx, conversationID, roundID, rec, agent.ToolOutput{Summary: rec.Summary})
			return agent.ToolOutput{}, err
		}
		rec.Summary = out.Summary
		recs = append(recs, rec)
		outputs = append(outputs, out)
		s.recordTool(ctx, conversationID, roundID, rec, out)
		return out, nil
	}

	isOverview := strings.Contains(text, "状态") || strings.Contains(text, "总览") ||
		strings.Contains(lower, "overview") || strings.Contains(text, "怎么样") ||
		strings.Contains(text, "如何")
	ns := s.resolveNamespace(ctx, text, history)

	// 创建 Deployment / Pod（Slice 10）：用户明确说要创建时，
	// 先问缺的信息（命名空间、名字、镜像），问全了直接给一张创建建议卡，
	// 不用先做故障诊断。裸 Pod 不直接创建，按 Deployment 给卡，
	// 并在回复里说明原因。放在镜像变更分支之前：
	// “创建……换成 nginx:1.25”首先是创建意图，不能当成换镜像处理。
	// 上一轮刚问过创建缺什么信息时，本轮即便只回了名字或镜像
	// （例如只回一句“叫 web-demo”），也是创建的继续，
	// 不能当成一个新问题去查故障。
	if isCreateIntent(text) || continuesCreation(history) {
		return s.runStubCreate(ctx, conversationID, roundID, text, history)
	}

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

	// 问“这个命名空间下有哪些 / 有几个 Pod”：直接列全部 Pod，
	// 不用先猜 Deployment，也不用走“找问题 Pod 再深挖”的诊断流程。
	// 问的是某一个 Deployment 下的 Pod 时也一样：直接列它下面的。
	// v0.14.0 新加的 list_pods 就是为这类问题准备的，
	// list_workload_pods 也改成返回完整行。
	if isListPodsQuestion(text) && ns != "" {
		if dep := deploymentMentionedInText(ctx, s, text, ns); dep != "" {
			wp, err := call("list_workload_pods", map[string]string{"namespace": ns, "deployment": dep})
			if err != nil {
				return &Reply{Reply: "列 Pod 时出错：" + err.Error(), ToolCalls: recs, Structured: structured}, nil
			}
			structured["workloadPods"] = wp.Data
			reply.WriteString(wp.Summary + "。")
			return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
		}
		pods, err := call("list_pods", map[string]string{"namespace": ns})
		if err != nil {
			return &Reply{Reply: "列 Pod 时出错：" + err.Error(), ToolCalls: recs, Structured: structured}, nil
		}
		structured["pods"] = pods.Data
		reply.WriteString(pods.Summary + "。")
		if countOf(pods.Data) == 0 {
			reply.WriteString("这个命名空间下没有 Pod。")
		} else {
			reply.WriteString("想看某个 Pod 是怎么回事，直接问我它的名字就行。")
		}
		return &Reply{Reply: reply.String(), ToolCalls: recs, Structured: structured}, nil
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

// ---------------------------------------------------------------------------
// 创建意图（Slice 10，Stub 的确定性规则，真模型路径不走这里，见 real.go）
// ---------------------------------------------------------------------------

// isListPodsQuestion 判断用户是不是在问“某命名空间下有哪些 / 有几个 Pod”：
// 问的是全部 Pod 的清单，不是“哪个 Pod 出了问题”，
// 所以用 list_pods 直接列，不走问题 Pod 深挖那条路。
// 提到具体 Deployment 名的也算（例如“demo-api 下有几个 Pod”），
// 只不过那种走 list_workload_pods，分支里再分。
func isListPodsQuestion(text string) bool {
	if !strings.Contains(text, "Pod") && !strings.Contains(text, "pod") {
		return false
	}
	return strings.Contains(text, "有哪些") || strings.Contains(text, "哪些") ||
		strings.Contains(text, "有几个") || strings.Contains(text, "多少个") ||
		strings.Contains(text, "几个") || strings.Contains(text, "列表")
}

// isCreateIntent 判断用户这轮是不是在说“创建 Deployment / Pod”：
// 消息里要有“创建”，并且提到 Deployment、部署或 Pod。
// “创建可审批的修复任务”是按钮文案，不是创建意图，
// 它不含 Deployment / Pod，不会误触发。
func isCreateIntent(text string) bool {
	if !strings.Contains(text, "创建") {
		return false
	}
	return strings.Contains(text, "Deployment") || strings.Contains(text, "部署") ||
		strings.Contains(text, "Pod") || strings.Contains(text, "pod")
}

// continuesCreation 判断本轮是不是上一轮创建追问的继续：
// 上一轮助手刚问过“创建一个 Deployment 还缺这些信息”，
// 用户本轮只回了名字或镜像（例如只回一句“叫 web-demo”），
// 消息里没有“创建”二字，也要走创建分支，不能当成新问题去查故障。
func continuesCreation(history []store.ConversationMessage) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			return strings.Contains(history[i].Content, "创建一个 Deployment 还缺这些信息")
		}
	}
	return false
}

var (
	// 名字：用户明说的叫法，例如“叫 web-demo”、“名字是 web”。
	createNameRe = regexp.MustCompile(`(?:叫|名字(?:是|叫)?|名称(?:是|叫)?)\s*([a-z0-9][a-z0-9-]*)`)
	// 名字：跟在类型后面的名字，例如“创建 Deployment web”。
	createNameAfterKindRe = regexp.MustCompile(`(?:Deployment|部署|Pod|pod)\s+([a-z0-9][a-z0-9-]*)`)
	// 副本数：例如“3 个副本”、“副本数 3”。
	createReplicasRe = regexp.MustCompile(`(\d+)\s*个(?:副本|实例)|副本(?:数)?\s*[:：]?\s*(\d+)`)
	// 镜像：明说的镜像字段，例如“镜像 nginx:1.25”、“镜像是 nginx”。
	createImageFieldRe = regexp.MustCompile(`(?:镜像|image)\s*[:：是]?\s*([a-z0-9][a-z0-9._/-]+)`)
	// 镜像：用户只回了一个裸镜像名，例如上一轮刚问过“用什么镜像”，这轮只回“nginx:1.25”。
	createBareImageRe = regexp.MustCompile(`^([a-z0-9][a-z0-9._/-]+)$`)
	// 镜像：类型前面的带路径镜像，例如“创建一个 docker.io/library/nginx Deployment”。
	createImageBeforeKindRe = regexp.MustCompile(`([a-z0-9][a-z0-9._/-]*[/.][a-z0-9._/-]*)\s*(?:Deployment|部署|Pod|pod)`)
)

// parseCreateName 从合并后的用户消息里解析要创建的 Deployment 名字。
// 注意：类型前面的词不算名字——“创建一个 nginx Deployment”里的 nginx
// 是镜像不是名字，名字必须明说（“叫 web-demo”）或跟在类型后面
// （“创建 Deployment web”），否则宁可问用户，也不要把镜像当成名字。
func parseCreateName(all string) string {
	if m := createNameRe.FindStringSubmatch(all); m != nil {
		return m[1]
	}
	if m := createNameAfterKindRe.FindStringSubmatch(all); m != nil {
		return m[1]
	}
	return ""
}

// parseCreateImage 从合并后的用户消息里解析镜像，优先级：
// 带 tag 的镜像名 > 明说的镜像字段 > 上一轮问过镜像后只回的裸镜像名 >
// 类型前面的带路径镜像。都不中时返回 ""，调用方问用户。
func parseCreateImage(all, text string, history []store.ConversationMessage) string {
	if m := imageTokenRe.FindStringSubmatch(all); m != nil {
		return m[1]
	}
	if m := createImageFieldRe.FindStringSubmatch(all); m != nil {
		return m[1]
	}
	askedImage := false
	for _, h := range history {
		if h.Role == "assistant" && strings.Contains(h.Content, "镜像") {
			askedImage = true
		}
	}
	if askedImage {
		if m := createBareImageRe.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
			return m[1]
		}
	}
	if m := createImageBeforeKindRe.FindStringSubmatch(all); m != nil {
		return m[1]
	}
	return ""
}

// parseCreateReplicas 从合并后的用户消息里解析副本数，没说时返回 0，
// 调用方默认 1，不用为副本数多问一轮。
func parseCreateReplicas(all string) int {
	m := createReplicasRe.FindStringSubmatch(all)
	if m == nil {
		return 0
	}
	for _, g := range m[1:] {
		if g == "" {
			continue
		}
		n := 0
		for _, c := range g {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// deploymentExists 看 list_deployments 的结构化结果里是否已有同名 Deployment。
func deploymentExists(data any, name string) bool {
	b, _ := json.Marshal(data)
	var v struct {
		Deployments []struct {
			Name string `json:"name"`
		} `json:"deployments"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return false
	}
	for _, d := range v.Deployments {
		if d.Name == name {
			return true
		}
	}
	return false
}

// runStubCreate 是 Stub 的创建路径：确定性规则，不调真模型。
// 流程：先把本轮和历史里的用户消息拼在一起解析（多轮追问的记忆），
// 缺命名空间、名字、镜像时先问，不猜；问全了查一下同名是否已存在，
// 再过 ValidateCreation，最后直接给一张创建建议卡，
// 不用先做故障诊断——要创建的东西本来就没有故障可诊。
// 裸 Pod 不直接创建：用户说“创建一个 Pod”时也按 Deployment 给卡，
// 并在回复里说明原因（Pod 挂了不会自动重建，也没有 rollout）。
func (s *Service) runStubCreate(ctx context.Context, conversationID, roundID, text string, history []store.ConversationMessage) (*Reply, error) {
	var recs []ToolCallRecord
	structured := map[string]any{}

	call := func(name string, args map[string]string) (agent.ToolOutput, error) {
		if args == nil {
			args = map[string]string{}
		}
		tc := &agent.ToolCall{ID: fmt.Sprintf("chat-call-%d", len(recs)+1), Name: name, Args: args}
		start := time.Now()
		out, err := s.executeChatTool(ctx, tc)
		rec := ToolCallRecord{ID: tc.ID, Name: name, Args: args, DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			rec.Error = err.Error()
			rec.Summary = "工具执行失败：" + err.Error()
			recs = append(recs, rec)
			s.recordTool(ctx, conversationID, roundID, rec, agent.ToolOutput{Summary: rec.Summary})
			return agent.ToolOutput{}, err
		}
		rec.Summary = out.Summary
		recs = append(recs, rec)
		s.recordTool(ctx, conversationID, roundID, rec, out)
		return out, nil
	}

	// 多轮追问的记忆：用户分几轮给的信息（名字、镜像）都在历史的用户消息里，
	// 拼在一起解析，不用用户把整句话重说一遍。工具消息不拼进来，
	// 它的 JSON 里也有 name / image 字段，会误解析。
	var userTexts []string
	for _, h := range history {
		if h.Role == "user" {
			userTexts = append(userTexts, h.Content)
		}
	}
	userTexts = append(userTexts, text)
	all := strings.Join(userTexts, " ")
	ns := s.resolveNamespace(ctx, all, history)
	name := parseCreateName(all)
	image := parseCreateImage(all, text, history)
	replicas := parseCreateReplicas(all)

	// 缺信息时先问，不猜：名字、镜像、命名空间缺哪个问哪个，
	// 副本数默认 1，不用问。
	var missing []string
	if ns == "" {
		missing = append(missing, "命名空间")
	}
	if name == "" {
		missing = append(missing, "名字")
	}
	if image == "" {
		missing = append(missing, "镜像")
	}
	if len(missing) > 0 {
		reply := fmt.Sprintf("创建一个 Deployment 还缺这些信息：%s。请告诉我，例如：命名空间 demo，名字叫 web-demo，镜像 nginx:1.25。", strings.Join(missing, "、"))
		return &Reply{Reply: reply, ToolCalls: recs, Structured: structured}, nil
	}

	spec := repair.NormalizeCreation(&agent.SuggestedCreation{
		Namespace: ns, Name: name, Image: image, Replicas: replicas,
		Reason: "用户要求创建 Deployment " + name,
	})
	// 同名已存在时不给卡：直接告诉用户已存在，建议改它而不是重复创建。
	if out, err := call("list_deployments", map[string]string{"namespace": ns}); err == nil {
		structured["deployments"] = out.Data
		if deploymentExists(out.Data, name) {
			reply := fmt.Sprintf("%s 命名空间下已经有一个同名的 Deployment %s，不用重复创建。如果你想改它（换镜像、调副本数），直接告诉我改成什么。", ns, name)
			return &Reply{Reply: reply, ToolCalls: recs, Structured: structured}, nil
		}
	}
	if err := repair.ValidateCreation(spec); err != nil {
		reply := "这个创建请求校验没通过：" + err.Error()
		return &Reply{Reply: reply, ToolCalls: recs, Structured: structured}, nil
	}
	structured["suggestedCreation"] = spec
	var b strings.Builder
	// 用户说的是 Pod 不是 Deployment：按 Deployment 给卡，并说明原因。
	if (strings.Contains(text, "Pod") || strings.Contains(text, "pod")) && !strings.Contains(text, "Deployment") && !strings.Contains(text, "部署") {
		b.WriteString("直接创建裸 Pod 不推荐：Pod 挂了不会自动重建，也没有 rollout；下面这张卡是按 Deployment 给的，同样一个容器、同一个镜像，Deployment 会帮你管着 Pod。\n")
	}
	b.WriteString(fmt.Sprintf("可以创建：%s/%s，镜像 %s，%d 个副本。这只是一张建议卡，不会自动执行——如果你想走正式流程，可以点“创建可审批的创建任务”，生成提案后人工审批再执行。", spec.Namespace, spec.Name, spec.Image, spec.Replicas))
	return &Reply{Reply: b.String(), ToolCalls: recs, Structured: structured, SuggestedCreation: spec}, nil
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
