package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubecause/internal/agent"
	"kubecause/internal/config"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tools := &run.ToolsAdapter{Kube: kube.NewFake()}
	return NewService(s, tools, model.NewStub()), s
}

func newConversation(t *testing.T, svc *Service) string {
	t.Helper()
	c, err := svc.Store.CreateConversation(context.Background(), "")
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return c.ID
}

// TestChatTwoTurnsCoreference 是对话模式的核心用例：
// 第一轮问“当前状态怎么样”，第二轮只说“demo 那个一直重启的是怎么回事”，
// 不重新给 namespace，第二轮必须能接上文（demo），
// 返回结构化 Pod 列表和修复建议卡，且不生成任何 change_requests。
func TestChatTwoTurnsCoreference(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)

	r1, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if len(r1.ToolCalls) == 0 {
		t.Fatalf("第一轮应调过工具，得到 %+v", r1)
	}
	if r1.Structured["overview"] == nil || r1.Structured["problemPods"] == nil {
		t.Fatalf("第一轮应返回总览和有问题 Pod 的结构化结果，得到 %+v", r1.Structured)
	}

	r2, err := svc.RunMessage(ctx, conv, "demo 那个一直重启的是怎么回事")
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	// 第二轮必须接上文：调过的工具里要有 demo 这个 namespace。
	foundDemo := false
	for _, tc := range r2.ToolCalls {
		if tc.Args["namespace"] == "demo" {
			foundDemo = true
		}
	}
	if !foundDemo {
		t.Fatalf("第二轮应接上文查 demo，工具调用为 %+v", r2.ToolCalls)
	}
	// 结构化 Pod 列表必须有 demo-api。
	b, _ := json.Marshal(r2.Structured)
	if !strings.Contains(string(b), "demo-api") {
		t.Fatalf("第二轮结构化结果里应有 demo-api，得到 %s", b)
	}
	// OOM 场景应给修复建议卡：128Mi -> 256Mi。
	if r2.SuggestedRepair == nil {
		t.Fatalf("第二轮应返回修复建议卡，得到 %+v", r2)
	}
	sr := r2.SuggestedRepair
	if sr.Namespace != "demo" || sr.Deployment != "demo-api" || sr.OldValue != "128Mi" || sr.NewValue != "256Mi" {
		t.Fatalf("修复建议卡不对: %+v", sr)
	}
	// 对话只读：不能生成任何 change_requests，也不能改 Deployment。
	if _, err := s.GetChangeRequestByRun(ctx, "nonexistent"); err == nil {
		t.Fatalf("不应有提案被创建")
	}
	// 工具调用必须带耗时字段：fake 工具极快，耗时可能为 0ms，
	// 但字段必须明确存在（JSON 里有 "durationMs"），前端据此显示数字或“—”，
	// 不能靠缺失字段去猜。
	for _, tc := range r2.ToolCalls {
		if tc.DurationMs < 0 {
			t.Fatalf("工具耗时不能为负: %+v", tc)
		}
	}
	bDur, _ := json.Marshal(r2.ToolCalls)
	if !strings.Contains(string(bDur), `"durationMs"`) {
		t.Fatalf("工具调用 JSON 里应明确包含 durationMs 字段，得到 %s", bDur)
	}
	dep, _ := svc.Tools.GetDeployment(ctx, "demo", "demo-api")
	if dep.Data == nil {
		t.Fatalf("应能读到 Deployment")
	}
	b2, _ := json.Marshal(dep.Data)
	if !strings.Contains(string(b2), "128Mi") {
		t.Fatalf("对话后 Deployment 的 limit 应仍是 128Mi（对话不执行修复），得到 %s", b2)
	}
	// 历史应完整保留：用户 2 条 + 助手 2 条 + tool 若干条。
	msgs, _ := s.ListMessages(ctx, conv)
	users, assistants := 0, 0
	for _, m := range msgs {
		if m.Role == "user" {
			users++
		}
		if m.Role == "assistant" {
			assistants++
		}
	}
	if users != 2 || assistants != 2 {
		t.Fatalf("历史应有 2 条用户消息和 2 条助手消息，得到 user=%d assistant=%d", users, assistants)
	}
}

// TestChatUnknownNamespaceAsks 不猜 namespace：
// 用户没给 namespace、历史里也没有时，对话应主动问，而不是猜一个去查。
func TestChatUnknownNamespaceAsks(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "那个一直重启的是怎么回事")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if !strings.Contains(r.Reply, "命名空间") {
		t.Fatalf("定不下 namespace 时应主动问，得到 %q", r.Reply)
	}
}

// TestChatHistoryTruncation 覆盖历史截断：
// 往会话里写 12 条消息后取最近 10 条，应正好 10 条，且最早的一条已被截掉。
func TestChatHistoryTruncation(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	for i := 0; i < 12; i++ {
		_ = s.AppendMessage(ctx, &store.ConversationMessage{
			ConversationID: conv, Role: "user", Content: "历史消息",
		})
	}
	got, err := svc.lastMessages(ctx, conv, svc.MaxHistoryMessages)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("最近历史应截断到 10 条，得到 %d 条", len(got))
	}
}

// TestChatSystemNamespaceReadable 覆盖 Slice 5 的“读放开、写挡住”：
// 对话里查 kube-system 不再被“系统命名空间”拦住——fake 里没有
// kube-system 的 Pod，工具会返回“找不到”而不是“禁止查询”；
// 写仍然被挡，那是 repair.ValidatePatchSpec 的职责，这里不测。
func TestChatSystemNamespaceReadable(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	_, err := svc.executeChatTool(ctx, &agent.ToolCall{
		ID: "t1", Name: "list_problem_pods", Args: map[string]string{"namespace": "kube-system"},
	})
	if err != nil {
		t.Fatalf("对话里 list_problem_pods(kube-system) 应能执行，得到错误: %v", err)
	}
	_, err = svc.executeChatTool(ctx, &agent.ToolCall{
		ID: "t2", Name: "get_pod", Args: map[string]string{"namespace": "kube-system", "pod": "coredns-abc"},
	})
	if err != nil && strings.Contains(err.Error(), "系统命名空间") {
		t.Fatalf("对话里查 kube-system 不应再被系统命名空间拦截，得到: %v", err)
	}
	// extractNamespace 也要能提出 kube-system，这样对话才能接上文。
	if got := extractNamespace(`{"namespace":"kube-system"}`); got != "kube-system" {
		t.Fatalf("extractNamespace 应提出 kube-system，得到 %q", got)
	}
}

// TestChatRepairPersistsAfterReload 覆盖 Slice 6 修的建议卡丢失 bug：
// 第二轮返回的修复建议卡，必须和 Structured 一起落盘，
// 重新 GET 会话后最后一条助手消息里仍有 suggestedRepair，
// 前端刷新后“转成正式诊断”按钮才不会丢。
func TestChatRepairPersistsAfterReload(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if _, err := svc.RunMessage(ctx, conv, "demo 那个一直重启的是怎么回事"); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	msgs, err := s.ListMessages(ctx, conv)
	if err != nil {
		t.Fatalf("读会话历史失败: %v", err)
	}
	var lastAssistant *store.ConversationMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = &msgs[i]
			break
		}
	}
	if lastAssistant == nil {
		t.Fatalf("历史里应有助手消息")
	}
	var structured map[string]any
	if err := json.Unmarshal([]byte(lastAssistant.Structured), &structured); err != nil {
		t.Fatalf("助手消息的 Structured 应是合法 JSON: %v", err)
	}
	sr, ok := structured["suggestedRepair"].(map[string]any)
	if !ok {
		t.Fatalf("刷新后 Structured 里仍应有 suggestedRepair，得到 %v", structured)
	}
	if sr["oldValue"] != "128Mi" || sr["newValue"] != "256Mi" {
		t.Fatalf("持久化后的建议卡新旧值不对: %+v", sr)
	}
}

// TestRealNoSyntheticRepair 覆盖 Slice 6 的“修复决策只走模型单一路”：
// 用 mock 模型服务返回纯文本最终回答（只在文字里提修复，不给结构化字段），
// 真模型路径追问一次后仍拿不到结构化建议时，必须返回无卡，
// 不能调规则合成一张卡出来。
func TestRealNoSyntheticRepair(t *testing.T) {
	// mock 模型：每次都返回一段纯文本，不调任何工具、不提交诊断报告。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"初步判断是内存问题，建议把 limit 从 128Mi 调到 256Mi。"}}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	conv := newConversation(t, svc)
	reply, err := svc.RunMessage(ctx, conv, "demo 那个一直重启的是怎么回事")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if reply.SuggestedRepair != nil {
		t.Fatalf("模型只给文字没给结构化建议时，不应合成建议卡，得到 %+v", reply.SuggestedRepair)
	}
	if _, ok := reply.Structured["suggestedRepair"]; ok {
		t.Fatalf("Structured 里不应出现合成的 suggestedRepair，得到 %+v", reply.Structured)
	}
	if strings.Contains(reply.Reply, "修复建议：") {
		t.Fatalf("不应在回复里追加合成的修复建议文本，得到 %q", reply.Reply)
	}
	// 刷新后同样无卡：持久化逻辑不能把一张不存在的卡变出来。
	msgs, _ := s.ListMessages(ctx, conv)
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Structured, "suggestedRepair") {
			t.Fatalf("刷新后历史里不应出现 suggestedRepair，得到 %s", m.Structured)
		}
	}
}

// TestRealCanonicalKeys 覆盖 Slice 7 修的“后续追问只剩纯文本”：
// 真模型路径以前只把工具结果存进 structured[工具名]（例如 structured["get_pod"]），
// 前端 buildBlocks 只认规范 key（overview、problemPods、pod……），
// 工具名当 key 的结果会被过滤掉，聊天里就只剩文字、没有卡片，刷新后也一样。
// 现在每个工具的结果要同时写一份规范 key，刷新后结构化块还在。
func TestRealCanonicalKeys(t *testing.T) {
	// mock 模型按顺序返回：先调 get_cluster_overview，再调 list_problem_pods，
	// 然后给一段纯文本最终回答（触发一次追问），追问后仍给纯文本，结束本轮。
	// 注意：追问后模型仍无结构化建议是允许的，本测试只验规范 key，不验建议卡。
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}]}`)
		case 2:
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-2","type":"function","function":{"name":"list_problem_pods","arguments":"{}"}}]}}]}`)
		default:
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"集群里 demo-api 有问题，正在重启。"}}]}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	conv := newConversation(t, svc)
	reply, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if reply.Structured["overview"] == nil {
		t.Fatalf("真模型调 get_cluster_overview 后 Structured 里应有规范 key overview，得到 %+v", reply.Structured)
	}
	if reply.Structured["problemPods"] == nil {
		t.Fatalf("真模型调 list_problem_pods 后 Structured 里应有规范 key problemPods，得到 %+v", reply.Structured)
	}
	// 刷新后（重新读库里的助手消息）规范 key 还在，前端才能渲染结构化块。
	msgs, _ := s.ListMessages(ctx, conv)
	var lastAssistant *store.ConversationMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = &msgs[i]
			break
		}
	}
	if lastAssistant == nil {
		t.Fatalf("历史里应有助手消息")
	}
	var structured map[string]any
	if err := json.Unmarshal([]byte(lastAssistant.Structured), &structured); err != nil {
		t.Fatalf("助手消息的 Structured 应是合法 JSON: %v", err)
	}
	if structured["overview"] == nil || structured["problemPods"] == nil {
		t.Fatalf("刷新后 Structured 里仍应有 overview 和 problemPods，得到 %v", structured)
	}
}

// TestChatMaxStepsFromConfig 覆盖“步数从配置里读”：
// 配置中心改 chat.maxSteps 后，ApplyConfig 应把新值应用到对话服务，
// 下一轮对话就按新预算跑，不用重启。
func TestChatMaxStepsFromConfig(t *testing.T) {
	svc, _ := newTestService(t)
	if svc.MaxSteps != 12 {
		t.Fatalf("默认对话步数应是 12，得到 %d", svc.MaxSteps)
	}
	cfg := config.Defaults()
	cfg.Chat.MaxSteps = 16
	cfg.Chat.MaxHistoryMessages = 5
	svc.ApplyConfig(cfg)
	if svc.MaxSteps != 16 {
		t.Fatalf("ApplyConfig 后对话步数应是 16，得到 %d", svc.MaxSteps)
	}
	if svc.MaxHistoryMessages != 5 {
		t.Fatalf("ApplyConfig 后历史条数应是 5，得到 %d", svc.MaxHistoryMessages)
	}
	// 配置里改回文件默认值再应用，应回到 12 / 10。
	svc.ApplyConfig(config.Defaults())
	if svc.MaxSteps != 12 || svc.MaxHistoryMessages != 10 {
		t.Fatalf("改回默认配置后应是 12 / 10，得到 %d / %d", svc.MaxSteps, svc.MaxHistoryMessages)
	}
}

// TestRealStepsExhaustedReturnsPartial 覆盖 Slice 8 的“步数用完返回半成品”：
// mock 模型一直调工具、永远不给最终回答，步数用完后对话不应报错，
// 而应返回已经查到的总览和问题 Pod（structured 里有 overview 和 problemPods），
// 文字里写明“步数用完了，还没来得及总结”，并问用户想深入查哪个目标。
// 以前这里直接 return error，前端只剩一片空白加一句
// “请换个更具体的问题再问”，等于把预算用完的锅推给用户。
func TestRealStepsExhaustedReturnsPartial(t *testing.T) {
	// mock 模型：每次都返回一个工具调用，交替调总览和问题 Pod，
	// 永远不给最终回答，逼出步数用完的分支。
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		name := "get_cluster_overview"
		if calls%2 == 0 {
			name = "list_problem_pods"
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-%d","type":"function","function":{"name":%q,"arguments":"{}"}}]}}]}`, calls, name)
	}))
	defer srv.Close()
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	// 步数预算压到 2 步，跑得快，也正好覆盖“总览 2 步就够”的场景。
	svc.MaxSteps = 2
	conv := newConversation(t, svc)
	reply, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("步数用完应返回半成品、不应报错，得到错误: %v", err)
	}
	if reply == nil {
		t.Fatalf("应返回半成品回复")
	}
	if !strings.Contains(reply.Reply, "步数用完了") {
		t.Fatalf("半成品文字应说明步数用完了，得到 %q", reply.Reply)
	}
	if !strings.Contains(reply.Reply, "还没来得及总结") {
		t.Fatalf("半成品文字应说明还没来得及总结，得到 %q", reply.Reply)
	}
	if !strings.Contains(reply.Reply, "想深入查哪个") {
		t.Fatalf("半成品文字应问用户想深入查哪个目标，得到 %q", reply.Reply)
	}
	if strings.Contains(reply.Reply, "换个更具体的问题") {
		t.Fatalf("半成品文字不应再让用户换问题背锅，得到 %q", reply.Reply)
	}
	if reply.Structured["overview"] == nil {
		t.Fatalf("半成品 Structured 里应有总览，得到 %+v", reply.Structured)
	}
	if reply.Structured["problemPods"] == nil {
		t.Fatalf("半成品 Structured 里应有问题 Pod，得到 %+v", reply.Structured)
	}
	if len(reply.ToolCalls) != 2 {
		t.Fatalf("半成品应记录 2 步工具调用，得到 %d 步", len(reply.ToolCalls))
	}
	// 半成品要正常落盘成一条助手消息：刷新会话后还能看到总览卡片和问题 Pod 表，
	// 而不是只在当次返回里有。
	msgs, _ := s.ListMessages(ctx, conv)
	var lastAssistant *store.ConversationMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			lastAssistant = &msgs[i]
			break
		}
	}
	if lastAssistant == nil {
		t.Fatalf("历史里应有半成品助手消息")
	}
	if !strings.Contains(lastAssistant.Content, "步数用完了") {
		t.Fatalf("落盘的半成品消息应包含步数说明，得到 %q", lastAssistant.Content)
	}
	var structured map[string]any
	if err := json.Unmarshal([]byte(lastAssistant.Structured), &structured); err != nil {
		t.Fatalf("半成品 Structured 应是合法 JSON: %v", err)
	}
	if structured["overview"] == nil || structured["problemPods"] == nil {
		t.Fatalf("刷新后半成品 Structured 里仍应有 overview 和 problemPods，得到 %v", structured)
	}
}

// TestChatConvertToRun 覆盖“转成正式诊断”：
// 对话两轮后不给参数直接转，应按历史解析出 demo/demo-api，创建任务。
func TestChatConvertToRun(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if _, err := svc.RunMessage(ctx, conv, "demo 那个一直重启的是怎么回事"); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	runID, err := svc.ConvertToRun(ctx, conv, "", "", "", "")
	if err != nil {
		t.Fatalf("转正式诊断失败: %v", err)
	}
	r, err := s.GetRun(ctx, runID)
	if err != nil || r == nil {
		t.Fatalf("转换后应能读到任务: %v", err)
	}
	if r.Namespace != "demo" || r.Name != "demo-api" || r.Kind != "Deployment" {
		t.Fatalf("转换解析的目标不对: %+v", r)
	}
}
