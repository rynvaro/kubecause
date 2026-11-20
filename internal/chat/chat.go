// Package chat 是对话模式的服务层（Slice 4）。
//
// 对话和单次诊断是两条线，共用同一套 Agent 核心、同一套工具、
// 同一套安全规则，但入口和状态不同：
//   - 单次诊断（/api/runs）：填 namespace / 类型 / 名字 / 问题，
//     任务异步跑，报告落盘，可审批、可执行、可验证；
//   - 对话（/api/conversations）：一问一答，Agent 记得上文，
//     默认只读，只能调只读工具，不能直接 Patch。
//     模型判断需要修复时，只返回一张结构化的修复建议卡，
//     不自动执行；用户点“创建可审批的修复任务”后，才走 /api/runs 流程，
//     生成提案、走人工审批、再执行和验证。对话里没有捷径能绕过审批。
//
// 多轮上下文：每次调用模型时带上本会话最近 10 条用户/助手消息，
// 只算 user 和 assistant，不算 tool。每次工具调用都会单独存一条
// tool 消息，一轮对话 = 1 条用户 + 6~8 条 tool + 1 条助手，
// 按全量行数算时“最近 10 条”实际只够覆盖一轮，问两三轮后最早那轮
// 的目标就被截掉了，模型只能按上一轮猜。tool 消息不占历史名额：
// 本轮调工具的结果由真模型循环实时追加进模型输入；上一轮的工具
// 细节丢掉，但助手消息里的结论和事实摘要还在，能接上文。
// 超出时只保留系统提示 + 最近 10 条用户/助手消息。10 条以后更早的
// 消息不会再进模型上下文，但数据库里全量保留，前端翻历史能看到。
// TODO：以后可把 10 条之前的历史做一次摘要，再把摘要放进系统提示，
// 现在不做假的摘要——没有摘要就直接截断，并在 README 里写清这一点。
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"kubecause/internal/agent"
	"kubecause/internal/config"
	"kubecause/internal/model"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// DefaultMaxChatSteps 是一轮对话里模型默认最多调工具的次数：12。
// MaxChatSteps 是预算，不是决策：调大它不干预模型怎么选工具，
// 只是允许模型在一轮里查得更多；步数用完时后端返回已查到的半成品，
// 而不是直接报错（见 real.go）。
// 单次诊断的预算是另外的 8 步（internal/run/worker.go + store 的默认值），
// 两条线不要混成一个数。
const DefaultMaxChatSteps = 12

// DefaultMaxHistoryMessages 是每次调用模型时默认带上的历史消息数：10，
// 只算用户和助手消息，tool 消息不占名额（见 lastMessages）。
const DefaultMaxHistoryMessages = 10

// ToolCallRecord 是一轮对话里调过的一个工具，
// 存进 messages.tool_calls，前端能看到每一步调了什么。
// DurationMs 是本次工具调用的耗时（毫秒），由调用方在执行前后计时；
// 工具极快导致计时为 0 时前端显示“—”，不编一个数字。
// 字段不用 omitempty：0 本身就是一个明确的值（快到不足 1ms）。
type ToolCallRecord struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Args       map[string]string `json:"args"`
	Summary    string            `json:"summary"`
	Error      string            `json:"error,omitempty"`
	DurationMs int64             `json:"durationMs"`
}

// Reply 是一轮对话的返回：给用户看的文本、调过的工具、
// 结构化结果（前端渲染表格/卡片用）、可选的修复建议卡。
type Reply struct {
	Reply             string                   `json:"reply"`
	ToolCalls         []ToolCallRecord         `json:"toolCalls"`
	Structured        map[string]any           `json:"structured,omitempty"`
	SuggestedRepair   *agent.SuggestedRepair   `json:"suggestedRepair,omitempty"`
	SuggestedCreation *agent.SuggestedCreation `json:"suggestedCreation,omitempty"`
}

// Service 持有对话所需的依赖：存储、只读工具、模型。
// MaxSteps / MaxHistoryMessages 不再是写死的常量，而是从配置中心读：
// 启动时 main.go 调 ApplyConfig 填进去，POST /api/config 热更新时再调一次。
// 字段是导出的，测试可以直接改；<=0 时回落到默认值，
// 防止配置中心出问题时对话一步工具都调不了。
type Service struct {
	Store *store.Store
	Tools agent.Tools
	Model agent.Model
	// MaxSteps 是一轮对话的工具步数预算，默认 12。
	MaxSteps int
	// MaxHistoryMessages 是每次调用模型带上的历史消息数，默认 10。
	MaxHistoryMessages int
}

// NewService 组装对话服务，步数和历史数先用默认值，
// main.go 启动时再用配置中心的值覆盖（见 ApplyConfig）。
func NewService(s *store.Store, tools agent.Tools, m agent.Model) *Service {
	return &Service{
		Store: s, Tools: tools, Model: m,
		MaxSteps: DefaultMaxChatSteps, MaxHistoryMessages: DefaultMaxHistoryMessages,
	}
}

// ApplyConfig 把配置中心的值应用到对话服务：热加载时调它，不用重启。
func (s *Service) ApplyConfig(c config.Config) {
	if c.Chat.MaxSteps > 0 {
		s.MaxSteps = c.Chat.MaxSteps
	}
	if c.Chat.MaxHistoryMessages > 0 {
		s.MaxHistoryMessages = c.Chat.MaxHistoryMessages
	}
}

// maxSteps 返回本轮可用的步数预算，<=0 时回落默认值。
func (s *Service) maxSteps() int {
	if s.MaxSteps <= 0 {
		return DefaultMaxChatSteps
	}
	return s.MaxSteps
}

// maxHistory 返回每次调用模型带上的历史消息数（只算用户/助手消息），
// <=0 时回落默认值。
func (s *Service) maxHistory() int {
	if s.MaxHistoryMessages <= 0 {
		return DefaultMaxHistoryMessages
	}
	return s.MaxHistoryMessages
}

// lastMessages 取一个会话最近 N 条用户/助手消息，给模型做上下文。
// 只算 user 和 assistant，不算 tool：每次工具调用都会单独存一条
// tool 消息，一轮对话 = 1 条用户 + 6~8 条 tool + 1 条助手，
// 按全量行数算时“最近 10 条”实际只够覆盖一轮，问两三轮后最早那轮
// 的目标就被截掉了。tool 消息的工具结果不占历史名额：本轮调工具的
// 结果由真模型循环实时追加进模型输入；上一轮的工具细节丢掉，
// 但助手消息里的结论和事实摘要还在，能接上文。
// resolveNamespace 用的也是这份历史（RunMessage 里传进去的那份），
// 同样按这个口径走，不会被 tool 消息挤掉。
func (s *Service) lastMessages(ctx context.Context, conversationID string, n int) ([]store.ConversationMessage, error) {
	all, err := s.Store.ListMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	kept := make([]store.ConversationMessage, 0, len(all))
	for _, m := range all {
		if m.Role == "user" || m.Role == "assistant" {
			kept = append(kept, m)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept, nil
}

// RunMessage 处理一条用户消息：先把用户消息落盘，
// 再跑一轮 Agent（Stub 走确定性规则，真模型走模型循环），
// 最后把助手回复落盘并返回。
// 对话只读：本函数里没有任何写集群的操作，
// 修复建议只以 SuggestedRepair 卡的形式返回，不生成 change_requests。
func (s *Service) RunMessage(ctx context.Context, conversationID, text string) (*Reply, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("消息不能为空")
	}
	if _, err := s.Store.GetConversation(ctx, conversationID); err != nil {
		return nil, fmt.Errorf("会话不存在: %w", err)
	}
	if err := s.Store.AppendMessage(ctx, &store.ConversationMessage{
		ConversationID: conversationID, Role: "user", Content: text,
	}); err != nil {
		return nil, err
	}
	history, err := s.lastMessages(ctx, conversationID, s.maxHistory())
	if err != nil {
		return nil, err
	}
	var reply *Reply
	switch m := s.Model.(type) {
	case *model.Real:
		reply, err = s.runReal(ctx, conversationID, text, history, m)
	case *model.Stub:
		reply, err = s.runStub(ctx, conversationID, text, history)
	default:
		// 未知模型类型时按 Stub 的确定性规则走，至少能查状态、
		// 不会因为模型类型不对就让对话直接报错。
		reply, err = s.runStub(ctx, conversationID, text, history)
	}
	if err != nil {
		return nil, err
	}
	// 建议卡持久化（Slice 6 修的 bug）：修复建议必须和 Structured 一起落盘，
	// 不能只放在 Reply 顶层。以前这里只存了 ToolCalls 和 Structured，
	// runStub 返回的 SuggestedRepair 只在 Reply 顶层，前端重新拉会话时就丢了，
	// 按钮也没了。runReal 是把 sr 放进 structured 里才留住的，两边不一致。
	// 现在统一在这里做：只要本轮给了建议，就写进 Structured，
	// stub 和 real 两条路都走同一个保存逻辑。
	if reply.SuggestedRepair != nil {
		if reply.Structured == nil {
			reply.Structured = map[string]any{}
		}
		reply.Structured["suggestedRepair"] = reply.SuggestedRepair
	}
	// 创建建议卡也一样：不写进 Structured，前端刷新会话就丢了卡、按钮也没了。
	if reply.SuggestedCreation != nil {
		if reply.Structured == nil {
			reply.Structured = map[string]any{}
		}
		reply.Structured["suggestedCreation"] = reply.SuggestedCreation
	}
	toolCallsJSON, _ := json.Marshal(reply.ToolCalls)
	structuredJSON, _ := json.Marshal(reply.Structured)
	if err := s.Store.AppendMessage(ctx, &store.ConversationMessage{
		ConversationID: conversationID, Role: "assistant", Content: reply.Reply,
		ToolCalls: string(toolCallsJSON), Structured: string(structuredJSON),
	}); err != nil {
		return nil, err
	}
	return reply, nil
}

// recordTool 把一次工具调用记进本轮记录，并以 tool 角色落盘一条消息，
// 这样会话历史里能看到每一步调了什么、返回了什么。
func (s *Service) recordTool(ctx context.Context, conversationID string, rec ToolCallRecord, out agent.ToolOutput) {
	b, _ := json.Marshal([]ToolCallRecord{rec})
	_ = s.Store.AppendMessage(ctx, &store.ConversationMessage{
		ConversationID: conversationID, Role: "tool",
		Content: rec.Summary, ToolCalls: string(b),
	})
}

// executeChatTool 按工具名分发一次对话里的工具调用，
// 先做对话专用的校验，再执行。校验规则：
//   - 工具名必须在 agent 的白名单里（未知工具直接拒绝）；
//   - 定向工具必须带 namespace，空时调用方要先填上上文解析到的 namespace；
//   - 读系统命名空间是允许的（只读），写操作本来就不在对话工具表里，
//     修复建议还要过 repair.ValidatePatchSpec 才会生成提案。
//
// 工具失败不让整轮对话崩掉：返回错误给模型/规则层，
// 由它决定换个工具重试，还是如实告诉用户查不到。
func (s *Service) executeChatTool(ctx context.Context, call *agent.ToolCall) (agent.ToolOutput, error) {
	// 先过 Agent 的通用校验（namespace 为 "" 时对话的发现工具放行，
	// 定向工具仍会被“缺少 namespace”拦住，调用方要先补齐）。
	if err := agent.ValidateToolCall("", call); err != nil {
		// ValidateToolCall 用 namespace="" 做任务上下文时，
		// 定向工具的“与任务不一致”检查会误伤（"" != "demo"），
		// 所以这里只认三类硬错误，其他放行到下面的分发再核对。
		if strings.Contains(err.Error(), "未知工具") || strings.Contains(err.Error(), "格式非法") {
			return agent.ToolOutput{}, err
		}
		if call.Name != "search_runbooks" && !isDiscovery(call.Name) {
			ns := call.Args["namespace"]
			if ns == "" {
				return agent.ToolOutput{}, fmt.Errorf("工具 %s 缺少 namespace 参数", call.Name)
			}
		}
	}
	switch call.Name {
	case "get_pod":
		return s.Tools.GetPod(ctx, call.Args["namespace"], call.Args["pod"])
	case "list_workload_pods":
		return s.Tools.ListWorkloadPods(ctx, call.Args["namespace"], call.Args["deployment"])
	case "get_events":
		return s.Tools.GetEvents(ctx, call.Args["namespace"], call.Args["name"])
	case "get_pod_logs":
		return s.Tools.GetPodLogs(ctx, call.Args["namespace"], call.Args["pod"], call.Args["container"], call.Args["previous"] == "true")
	case "get_deployment":
		return s.Tools.GetDeployment(ctx, call.Args["namespace"], call.Args["deployment"])
	case "search_runbooks":
		return s.Tools.SearchRunbooks(ctx, call.Args["query"])
	case "list_namespaces":
		return s.Tools.ListNamespaces(ctx)
	case "list_deployments":
		return s.Tools.ListDeployments(ctx, call.Args["namespace"])
	case "list_problem_pods":
		return s.Tools.ListProblemPods(ctx, call.Args["namespace"])
	case "get_cluster_overview":
		return s.Tools.GetClusterOverview(ctx)
	default:
		return agent.ToolOutput{}, fmt.Errorf("未知工具: %s", call.Name)
	}
}

func isDiscovery(name string) bool {
	switch name {
	case "list_namespaces", "list_deployments", "list_problem_pods", "get_cluster_overview":
		return true
	}
	return false
}

// extractNamespace 从一段 JSON 或文本里提取第一个 namespace 值。
// 系统命名空间也照常提取：读是允许的，提取出来才能让对话接上文。
func extractNamespace(s string) string {
	if m := nsJSONRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// resolveNamespace 从用户消息和会话历史里解析目标 namespace：
// 先看消息里是否明确提到了一个已知的 namespace，
// 再看历史里最近提到的 namespace，最后返回 "" 表示还没定，
// 调用方要主动问用户，而不是猜一个。
func (s *Service) resolveNamespace(ctx context.Context, text string, history []store.ConversationMessage) string {
	names := []string{}
	if out, err := s.Tools.ListNamespaces(ctx); err == nil {
		if data, ok := out.Data.(map[string]any); ok {
			if list, ok := data["namespaces"].([]string); ok {
				names = list
			}
		}
	}
	// ListNamespaces 的 Data 是 map[string]any，namespaces 是 []string，
	// 上面断言失败时（例如测试用的桩工具返回 []any）再按 JSON 走一遍。
	if len(names) == 0 {
		if out, err := s.Tools.ListNamespaces(ctx); err == nil {
			b, _ := json.Marshal(out.Data)
			var v struct {
				Namespaces []string `json:"namespaces"`
			}
			_ = json.Unmarshal(b, &v)
			names = v.Namespaces
		}
	}
	for _, n := range names {
		if n != "" && strings.Contains(text, n) {
			return n
		}
	}
	// 历史里最近提到的 namespace：从后往前找第一条带 namespace 的记录。
	for i := len(history) - 1; i >= 0; i-- {
		if ns := extractNamespace(history[i].Content + " " + history[i].ToolCalls + " " + history[i].Structured); ns != "" {
			return ns
		}
	}
	// 兜底：消息里出现 "demo" 这个词时，demo 就是目标。
	// 这是 fake 演示数据的 namespace，真集群上 ListNamespaces 已覆盖，
	// 这里只是防止测试桩工具没返回 namespace 列表时解析不到。
	if strings.Contains(text, "demo") {
		return "demo"
	}
	return ""
}

var nsJSONRe = regexp.MustCompile(`"namespace"\s*:\s*"([^"]+)"`)

// ConvertInput 是转正式诊断时前端传来的目标和建议（Slice 10）。
// 建议卡上的转换按钮把这张卡的目标（namespace/kind/name）和
// Patches（或创建规格）一起传过来，后端优先用它们，
// 不再去历史里猜目标——以前只传会话 id，后端去历史里猜，
// 猜到的是最早的 demo-api，demo-bad-image 的卡转出来
// 却是 demo-api 的任务。什么都没传时才回退到历史猜测（老客户端兼容）。
type ConvertInput struct {
	Namespace string
	Kind      string // Pod 或 Deployment
	Name      string
	Container string // 建议卡指明的容器名，生成提案时按它重算 Patch 下标
	Question  string
	// Patches 是建议卡上的通用 Patch 列表：带过来存在 Run 上，
	// Worker 看到它就直接校验、生成提案，不用重新诊断一遍。
	Patches []store.PatchOperation
	// Creation 是创建建议卡上的创建规格：带过来存在 Run 上，
	// Worker 看到它就直接校验、生成创建提案，同样不用重新诊断。
	Creation *store.CreationSpec
}

// ConvertToRun 把一次对话转成正式诊断任务（前端按钮叫“创建可审批的修复任务”）：
// 按前端传来的目标和建议、或会话历史解析出目标（namespace / kind / name / 问题），
// 创建 /api/runs 的任务并返回 runId。转换本身不执行修复，
// 修复还是要走提案 + 人工审批 + 执行验证那条线。
func (s *Service) ConvertToRun(ctx context.Context, conversationID string, in ConvertInput) (string, error) {
	if _, err := s.Store.GetConversation(ctx, conversationID); err != nil {
		return "", fmt.Errorf("会话不存在: %w", err)
	}
	history, err := s.Store.ListMessages(ctx, conversationID)
	if err != nil {
		return "", err
	}
	// 创建建议优先：目标就是要创建的 Deployment，问题也按创建来写。
	// 创建规格先过一遍校验，校验不过直接报错，不建一个注定没提案的任务。
	if in.Creation != nil {
		spec := repair.NormalizeCreation(in.Creation)
		if err := repair.ValidateCreation(spec); err != nil {
			return "", fmt.Errorf("创建建议校验没通过: %w", err)
		}
		question := in.Question
		if question == "" {
			question = fmt.Sprintf("创建 Deployment %s/%s（镜像 %s，%d 个副本）", spec.Namespace, spec.Name, spec.Image, spec.Replicas)
		}
		creationJSON, _ := json.Marshal(spec)
		r := &store.Run{Namespace: spec.Namespace, Kind: "Deployment", Name: spec.Name, Question: question, CreationJSON: string(creationJSON)}
		if err := s.Store.CreateRun(ctx, r); err != nil {
			return "", err
		}
		return r.ID, nil
	}
	namespace := in.Namespace
	if namespace == "" {
		namespace = s.resolveNamespace(ctx, lastUserText(history), history)
	}
	if namespace == "" {
		return "", fmt.Errorf("解析不到目标 namespace，请在转换时明确指定 namespace")
	}
	kind, name := in.Kind, in.Name
	if name == "" || kind == "" {
		// 只有前端什么都没传时才回退到历史猜测（老客户端兼容），
		// 传了目标的以传来的为准，不再猜。
		k, n := resolveTargetFromHistory(ctx, s, history, namespace)
		if kind == "" {
			kind = k
		}
		if name == "" {
			name = n
		}
	}
	if kind != "Pod" && kind != "Deployment" {
		kind = "Deployment"
	}
	if name == "" {
		return "", fmt.Errorf("解析不到目标名称，请在转换时明确指定 name")
	}
	question := in.Question
	if question == "" {
		question = lastUserText(history)
		if question == "" {
			question = fmt.Sprintf("%s 为什么有问题", name)
		}
	}
	r := &store.Run{Namespace: namespace, Kind: kind, Name: name, Question: question}
	// 建议卡的 Patches 跟着任务走：Worker 看到它就直接校验、生成提案，
	// 不用重新诊断一遍（以前重跑 8 步耗尽、置信度 low、没有提案）。
	if len(in.Patches) > 0 {
		sr := &agent.SuggestedRepair{
			Namespace: namespace, Deployment: name, Container: in.Container,
			Patches: in.Patches,
		}
		sr.Normalize()
		b, _ := json.Marshal(sr)
		r.SuggestedJSON = string(b)
	}
	if err := s.Store.CreateRun(ctx, r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func lastUserText(history []store.ConversationMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Content
		}
	}
	return ""
}

// resolveTargetFromHistory 从历史里找最近的问题 Pod / Deployment：
// 优先看结构化结果里的 pods 列表，第一条就是最近发现的问题 Pod，
// 再从它的 deployment 字段取 Deployment 名；
// 没有结构化结果时，看用户消息里是否提到了已知的 Deployment 名。
func resolveTargetFromHistory(ctx context.Context, s *Service, history []store.ConversationMessage, namespace string) (kind, name string) {
	for i := len(history) - 1; i >= 0; i-- {
		var v struct {
			Pods []struct {
				Name       string `json:"name"`
				Deployment string `json:"deployment"`
				Namespace  string `json:"namespace"`
			} `json:"pods"`
		}
		if history[i].Structured != "" {
			if err := json.Unmarshal([]byte(history[i].Structured), &v); err == nil && len(v.Pods) > 0 {
				p := v.Pods[0]
				if p.Deployment != "" {
					return "Deployment", p.Deployment
				}
				return "Pod", p.Name
			}
		}
	}
	// 再看用户消息里是否提到了 Deployment 名。
	if out, err := s.Tools.ListDeployments(ctx, namespace); err == nil {
		b, _ := json.Marshal(out.Data)
		var v struct {
			Deployments []struct {
				Name string `json:"name"`
			} `json:"deployments"`
		}
		if err := json.Unmarshal(b, &v); err == nil {
			text := ""
			for _, m := range history {
				text += " " + m.Content
			}
			for _, d := range v.Deployments {
				if d.Name != "" && strings.Contains(text, d.Name) {
					return "Deployment", d.Name
				}
			}
			if len(v.Deployments) > 0 {
				return "Deployment", v.Deployments[0].Name
			}
		}
	}
	return "", ""
}

// synthesizeRepair 从已收集的工具结果里合成一张修复建议卡。
// 注意：这只是桩模型（Stub）测试路径用的确定性规则，不是产品逻辑。
// 真模型路径（runReal）不调它：修复决策只能由模型做，后端只做校验。
// 即便有 OOM 这个案例，也不要为了演示写死——规则合成永远只是测试替身。
// 只有证据明确指向 OOMKilled、且能读到 Deployment 的当前内存 limit 时才给内存卡，
// 或证据明确指向 ImagePullBackOff、且当前镜像是 fake 场景的坏 tag 时才给镜像卡，
// 新值是旧值翻倍（封顶 4Gi）或 fake 场景的好镜像，再过一次 ValidatePatch，
// 不通过就不给卡。合成的卡和模型直接给的卡一样，
// 都只是建议，不生成 change_requests，不执行。
// 两种卡都走同一套通用 Patch 格式，不再只认内存。
func synthesizeRepair(ctx context.Context, s *Service, outputs []agent.ToolOutput, namespace, deployment string) *agent.SuggestedRepair {
	joined := ""
	var limit, container, image string
	for _, out := range outputs {
		joined += out.Summary + " "
		// 一手证据优先：search_runbooks 的片段是手册内容，不是现场数据，
		// 不能让它参与信号判断——例如查 ImagePullBackOff 时，
		// 手册片段里可能提到 OOM，会把镜像案误判成内存案。
		// 检索结果只看摘要（摘要里只有命中了哪篇手册），不看片段全文。
		if data, ok := out.Data.(map[string]any); ok {
			if _, isRunbook := data["snippets"]; isRunbook {
				continue
			}
		}
		b, _ := json.Marshal(out.Data)
		joined += string(b) + " "
		if data, ok := out.Data.(map[string]any); ok {
			if containers, ok := data["containers"].([]map[string]string); ok && len(containers) > 0 {
				limit = containers[0]["memoryLimit"]
				container = containers[0]["name"]
				image = containers[0]["image"]
			}
		}
	}
	if namespace == "" || deployment == "" {
		return nil
	}
	// 容器、limit 和镜像以集群现状为准：建议卡上的旧值必须和现状一致，
	// 否则转成正式诊断时 Worker 会直接放弃生成提案。
	if limit == "" || container == "" || image == "" {
		if depOut, err := s.Tools.GetDeployment(ctx, namespace, deployment); err == nil {
			if data, ok := depOut.Data.(map[string]any); ok {
				if containers, ok := data["containers"].([]map[string]string); ok && len(containers) > 0 {
					if limit == "" {
						limit = containers[0]["memoryLimit"]
					}
					if container == "" {
						container = containers[0]["name"]
					}
					if image == "" {
						image = containers[0]["image"]
					}
				}
			}
		}
	}
	if container == "" {
		return nil
	}
	isOOM := strings.Contains(joined, "OOMKilled") || strings.Contains(joined, "out of memory")
	isImagePull := strings.Contains(joined, "ImagePullBackOff") || strings.Contains(joined, "Failed to pull image")
	if isOOM && limit != "" {
		newValue := repair.DoubleMemory(limit, repair.MaxMemoryMi)
		if newValue == "" {
			return nil
		}
		sr := &agent.SuggestedRepair{
			Namespace: namespace, Deployment: deployment, Container: container,
			Field: "memoryLimit", OldValue: limit, NewValue: newValue,
			Reason: "容器内存限制过低，触发 OOMKilled 导致反复重启",
		}
		sr.Normalize()
		if err := repair.ValidatePatchSpec(sr.Namespace, sr.Deployment, sr.Container, sr.OldValue, sr.NewValue); err != nil {
			return nil
		}
		return sr
	}
	// 镜像卡：只在 fake 场景的坏 tag（demo-api:bad-tag）上给，
	// 新镜像是 fake 场景的好镜像 demo-api:latest。
	// 真集群上的坏镜像 tag 各不相同，新镜像只能由模型或用户决定，
	// 规则合成猜不出一个正确的 tag，这里不猜。
	if isImagePull && image == "demo-api:bad-tag" {
		sr := &agent.SuggestedRepair{
			Namespace: namespace, Deployment: deployment, Container: container,
			Field: "image", OldValue: image, NewValue: "demo-api:latest",
			Reason: "镜像 tag 不存在，拉取失败导致 Pod 卡在 ImagePullBackOff",
		}
		sr.Normalize()
		if err := repair.ValidatePatch(sr.Namespace, sr.Deployment, sr.Patches); err != nil {
			return nil
		}
		return sr
	}
	return nil
}
