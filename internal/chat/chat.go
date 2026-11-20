// Package chat 是对话模式的服务层（Slice 4）。
//
// 对话和单次诊断是两条线，共用同一套 Agent 核心、同一套工具、
// 同一套安全规则，但入口和状态不同：
//   - 单次诊断（/api/runs）：填 namespace / 类型 / 名字 / 问题，
//     任务异步跑，报告落盘，可审批、可执行、可验证；
//   - 对话（/api/conversations）：一问一答，Agent 记得上文，
//     默认只读，只能调只读工具，不能直接 Patch。
//     模型判断需要修复时，只返回一张结构化的修复建议卡，
//     不自动执行；用户点“转成正式诊断”后，才走 /api/runs 流程，
//     生成提案、走人工审批、再执行和验证。对话里没有捷径能绕过审批。
//
// 多轮上下文：每次调用模型时带上本会话最近 10 条消息，
// 超出时只保留系统提示 + 最近 10 条。10 条以后更早的消息
// 不会再进模型上下文，但数据库里全量保留，前端翻历史能看到。
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
	"kubecause/internal/model"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// MaxHistoryMessages 是每次调用模型时带上的历史消息上限。
const MaxHistoryMessages = 10

// MaxChatSteps 是一轮对话里模型最多调工具的次数，
// 和单次诊断的 8 步上限一致，防止模型无限循环烧 token。
const MaxChatSteps = 8

// ToolCallRecord 是一轮对话里调过的一个工具，
// 存进 messages.tool_calls，前端能看到每一步调了什么。
type ToolCallRecord struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Args    map[string]string `json:"args"`
	Summary string            `json:"summary"`
	Error   string            `json:"error,omitempty"`
}

// Reply 是一轮对话的返回：给用户看的文本、调过的工具、
// 结构化结果（前端渲染表格/卡片用）、可选的修复建议卡。
type Reply struct {
	Reply           string                 `json:"reply"`
	ToolCalls       []ToolCallRecord       `json:"toolCalls"`
	Structured      map[string]any         `json:"structured,omitempty"`
	SuggestedRepair *agent.SuggestedRepair `json:"suggestedRepair,omitempty"`
}

// Service 持有对话所需的依赖：存储、只读工具、模型。
type Service struct {
	Store *store.Store
	Tools agent.Tools
	Model agent.Model
}

// NewService 组装对话服务。
func NewService(s *store.Store, tools agent.Tools, m agent.Model) *Service {
	return &Service{Store: s, Tools: tools, Model: m}
}

// lastMessages 取一个会话最近 N 条消息，给模型做上下文。
func (s *Service) lastMessages(ctx context.Context, conversationID string, n int) ([]store.ConversationMessage, error) {
	all, err := s.Store.ListMessages(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
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
	history, err := s.lastMessages(ctx, conversationID, MaxHistoryMessages)
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
//   - 一律禁止系统命名空间；
//   - 发现工具的 namespace 为空表示查全部，合法。
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
		if strings.Contains(err.Error(), "未知工具") || strings.Contains(err.Error(), "系统命名空间") || strings.Contains(err.Error(), "格式非法") {
			return agent.ToolOutput{}, err
		}
		if call.Name != "search_runbooks" && !isDiscovery(call.Name) {
			ns := call.Args["namespace"]
			if ns == "" {
				return agent.ToolOutput{}, fmt.Errorf("工具 %s 缺少 namespace 参数", call.Name)
			}
			if isSystemNS(ns) {
				return agent.ToolOutput{}, fmt.Errorf("禁止查询系统命名空间 %q", ns)
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

func isSystemNS(ns string) bool {
	return ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease"
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

// extractNamespace 从一段 JSON 或文本里提取第一个 namespace 值。
func extractNamespace(s string) string {
	if m := nsJSONRe.FindStringSubmatch(s); m != nil && !isSystemNS(m[1]) {
		return m[1]
	}
	return ""
}

// ConvertToRun 把一次对话转成正式诊断任务：
// 按参数或会话历史解析出目标（namespace / kind / name / 问题），
// 创建 /api/runs 的任务并返回 runId。转换本身不执行修复，
// 修复还是要走提案 + 人工审批 + 执行验证那条线。
func (s *Service) ConvertToRun(ctx context.Context, conversationID, namespace, kind, name, question string) (string, error) {
	if _, err := s.Store.GetConversation(ctx, conversationID); err != nil {
		return "", fmt.Errorf("会话不存在: %w", err)
	}
	history, err := s.Store.ListMessages(ctx, conversationID)
	if err != nil {
		return "", err
	}
	if namespace == "" {
		namespace = s.resolveNamespace(ctx, lastUserText(history), history)
	}
	if namespace == "" {
		return "", fmt.Errorf("解析不到目标 namespace，请在转换时明确指定 namespace")
	}
	if name == "" || kind == "" {
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
	if question == "" {
		question = lastUserText(history)
		if question == "" {
			question = fmt.Sprintf("%s 为什么有问题", name)
		}
	}
	r := &store.Run{Namespace: namespace, Kind: kind, Name: name, Question: question}
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

// synthesizeRepair 从已收集的工具结果里合成一张修复建议卡：
// 只有证据明确指向 OOMKilled、且能读到 Deployment 的当前内存 limit 时才给，
// 新值是旧值翻倍（封顶 4Gi），再过一次 ValidatePatchSpec，
// 不通过就不给卡。合成的卡和模型直接给的卡一样，
// 都只是建议，不生成 change_requests，不执行。
func synthesizeRepair(ctx context.Context, s *Service, outputs []agent.ToolOutput, namespace, deployment string) *agent.SuggestedRepair {
	joined := ""
	var limit, container string
	for _, out := range outputs {
		joined += out.Summary + " "
		b, _ := json.Marshal(out.Data)
		joined += string(b) + " "
		if data, ok := out.Data.(map[string]any); ok {
			if containers, ok := data["containers"].([]map[string]string); ok && len(containers) > 0 {
				limit = containers[0]["memoryLimit"]
				container = containers[0]["name"]
			}
		}
	}
	if !strings.Contains(joined, "OOMKilled") && !strings.Contains(joined, "out of memory") {
		return nil
	}
	if namespace == "" || deployment == "" {
		return nil
	}
	// 容器和 limit 以集群现状为准：建议卡上的旧值必须和现状一致，
	// 否则转成正式诊断时 Worker 会直接放弃生成提案。
	if limit == "" || container == "" {
		if depOut, err := s.Tools.GetDeployment(ctx, namespace, deployment); err == nil {
			if data, ok := depOut.Data.(map[string]any); ok {
				if containers, ok := data["containers"].([]map[string]string); ok && len(containers) > 0 {
					limit = containers[0]["memoryLimit"]
					container = containers[0]["name"]
				}
			}
		}
	}
	if limit == "" || container == "" {
		return nil
	}
	newValue := repair.DoubleMemory(limit, repair.MaxMemoryMi)
	if newValue == "" {
		return nil
	}
	sr := &agent.SuggestedRepair{
		Namespace: namespace, Deployment: deployment, Container: container,
		Field: "memoryLimit", OldValue: limit, NewValue: newValue,
		Reason: "容器内存限制过低，触发 OOMKilled 导致反复重启",
	}
	if err := repair.ValidatePatchSpec(sr.Namespace, sr.Deployment, sr.Container, sr.OldValue, sr.NewValue); err != nil {
		return nil
	}
	return sr
}
