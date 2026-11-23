package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/config"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// seedMessages 直接往会话里写 n 条 user/assistant 消息（交替），
// 绕开 RunMessage——触发与游标只和消息条数有关，不需要真的跑一轮对话。
// 返回每条消息的 id（按时间顺序），游标断言用它。
func seedMessages(t *testing.T, svc *Service, conv string, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		content := fmt.Sprintf("问题 %d：demo-api 为什么重启", i+1)
		if i%2 == 1 {
			role = "assistant"
			content = fmt.Sprintf("回复 %d：查到 OOMKilled，limit 128Mi，证据 ev-%d", i+1, i+1)
		}
		m := &store.ConversationMessage{ConversationID: conv, Role: role, Content: content}
		if err := svc.Store.AppendMessage(ctx, m); err != nil {
			t.Fatalf("写消息失败: %v", err)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func TestFormatSummaryLinesIncludesStructuredEvidence(t *testing.T) {
	// review 点名的情形：Content 里没有 ev-12，但
	// Structured.report.evidenceIds 里有，摘要输入必须含该 ID。
	// formatReportReply 的展示文本不含 EvidenceIDs，证据编号
	// 只存在 Structured 里，不从这里提取就永远丢给摘要模型。
	st, _ := json.Marshal(map[string]any{
		"report": map[string]any{"evidenceIds": []string{"ev-12", "ev-3"}},
	})
	m := store.ConversationMessage{
		Role:       "assistant",
		Content:    "诊断结论：demo-api 内存不足。",
		Structured: string(st),
	}
	joined := strings.Join(formatSummaryLines([]store.ConversationMessage{m}), "\n")
	if !strings.Contains(joined, "ev-12") || !strings.Contains(joined, "ev-3") {
		t.Fatalf("摘要输入应包含 Structured 里的证据编号，得到 %q", joined)
	}
	// 去重：Content 里已有的编号不重复写。
	m2 := store.ConversationMessage{
		Role:       "assistant",
		Content:    "诊断结论：demo-api 内存不足，证据 ev-12。",
		Structured: string(st),
	}
	joined2 := strings.Join(formatSummaryLines([]store.ConversationMessage{m2}), "\n")
	if strings.Count(joined2, "ev-12") != 1 {
		t.Fatalf("Content 里已有的 ev-12 不应重复，得到 %q", joined2)
	}
	if !strings.Contains(joined2, "ev-3") {
		t.Fatalf("Content 里没有的 ev-3 仍应补上，得到 %q", joined2)
	}
	// 子串陷阱：Content 里有 ev-12 时，去重不能把 ev-1 也误滤掉——
	// 必须按 ev-<数字> 整体匹配。补充行应恰好是“证据 ev-1”。
	st2, _ := json.Marshal(map[string]any{
		"report": map[string]any{"evidenceIds": []string{"ev-1", "ev-12"}},
	})
	m2b := store.ConversationMessage{
		Role:       "assistant",
		Content:    "诊断结论：demo-api 内存不足，证据 ev-12。",
		Structured: string(st2),
	}
	lines2b := formatSummaryLines([]store.ConversationMessage{m2b})
	if len(lines2b) != 2 || lines2b[1] != "（结构化补充：证据 ev-1）" {
		t.Fatalf("ev-1 不应被 ev-12 的子串匹配误滤，得到 %q", lines2b)
	}
	// 边界：Structured 为空或非法 JSON 时不报错、不阻塞，
	// 摘要输入退回纯 Content。
	for _, bad := range []string{"", "{invalid json", "[]"} {
		m3 := store.ConversationMessage{Role: "assistant", Content: "诊断结论：demo-api 内存不足。", Structured: bad}
		lines := formatSummaryLines([]store.ConversationMessage{m3})
		if len(lines) != 1 || lines[0] != "助手：诊断结论：demo-api 内存不足。" {
			t.Fatalf("Structured=%q 时应退回纯 Content，得到 %q", bad, lines)
		}
	}
	// 建议卡：Content 里没有卡文本时，从 Structured 补目标；
	// Content 里已有卡文本时不再重复堆叠。
	sr, _ := json.Marshal(map[string]any{
		"suggestedRepair": map[string]any{
			"namespace": "demo", "deployment": "demo-api", "container": "demo-api",
			"patches": []map[string]any{{"op": "replace", "path": "/spec/template/spec/containers/0/resources/limits/memory", "value": "256Mi"}},
			"reason": "内存不足",
		},
	})
	m4 := store.ConversationMessage{
		Role:       "assistant",
		Content:    "诊断结论：demo-api 内存不足。",
		Structured: string(sr),
	}
	joined4 := strings.Join(formatSummaryLines([]store.ConversationMessage{m4}), "\n")
	if !strings.Contains(joined4, "demo/demo-api") || !strings.Contains(joined4, "256Mi") {
		t.Fatalf("Content 缺卡文本时应从 Structured 补建议卡目标，得到 %q", joined4)
	}
	m5 := store.ConversationMessage{
		Role:       "assistant",
		Content:    "诊断结论：demo-api 内存不足。\n修复建议：demo/demo-api 容器 demo-api：/spec/template/spec/containers/0/resources/limits/memory -> 256Mi（内存不足）。",
		Structured: string(sr),
	}
	joined5 := strings.Join(formatSummaryLines([]store.ConversationMessage{m5}), "\n")
	if strings.Count(joined5, "demo/demo-api") != 1 {
		t.Fatalf("Content 里已有的建议卡目标不应重复，得到 %q", joined5)
	}
}

func TestSummaryNotTriggeredForShortConversation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	seedMessages(t, svc, conv, 10)
	if got := svc.maybeSummarize(ctx, conv); got != "" {
		t.Fatalf("10 条消息不应触发摘要，得到 %q", got)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.Summary != "" || c.SummaryCursor != 0 {
		t.Fatalf("没触发时库里不应有摘要，得到 %+v", c)
	}
}

func TestSummaryBoundary(t *testing.T) {
	// 边界：19 条时未覆盖 9 条，不触发；再加 1 条到 20 条，
	// 未覆盖 10 条，触发，游标推到第 10 条消息。
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	ids := seedMessages(t, svc, conv, 19)
	if got := svc.maybeSummarize(ctx, conv); got != "" {
		t.Fatalf("19 条消息不应触发摘要，得到 %q", got)
	}
	seedMessages(t, svc, conv, 1)
	got := svc.maybeSummarize(ctx, conv)
	if got == "" {
		t.Fatalf("20 条消息时应触发摘要")
	}
	if !strings.Contains(got, "已总结 10 条") {
		t.Fatalf("首次摘要应覆盖 10 条未覆盖消息，得到 %q", got)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.SummaryCursor != ids[9] {
		t.Fatalf("游标应推到第 10 条消息的 id %d，得到 %d", ids[9], c.SummaryCursor)
	}
}

func TestSummaryIncremental(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	ids1 := seedMessages(t, svc, conv, 20)
	first := svc.maybeSummarize(ctx, conv)
	if !strings.Contains(first, "已总结 10 条") {
		t.Fatalf("首次摘要应覆盖 10 条，得到 %q", first)
	}
	// 再攒 10 条：第二次只总结新增段，游标推到第 20 条。
	seedMessages(t, svc, conv, 10)
	second := svc.maybeSummarize(ctx, conv)
	if strings.Count(second, "测试摘要") != 2 {
		t.Fatalf("第二次摘要应接上旧摘要、共 2 个标记，得到 %q", second)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.SummaryCursor != ids1[19] {
		t.Fatalf("第二次游标应推到第 20 条消息的 id %d，得到 %d", ids1[19], c.SummaryCursor)
	}
	// 第三次调用：未覆盖不足阈值，不再触发，摘要原样返回。
	if got := svc.maybeSummarize(ctx, conv); got != second {
		t.Fatalf("未攒够阈值时应原样返回已有摘要，得到 %q", got)
	}
}

func TestSummaryFailureKeepsOld(t *testing.T) {
	// 先用 Stub 生成一份摘要，再把模型换成缺 Key 的真模型：
	// 摘要调用失败时聊天不受影响，旧摘要和游标原样不动。
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	seedMessages(t, svc, conv, 20)
	old := svc.maybeSummarize(ctx, conv)
	if old == "" {
		t.Fatalf("应先生成一份摘要")
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	oldCursor := c.SummaryCursor
	svc.Model = model.NewReal("")
	seedMessages(t, svc, conv, 10)
	if got := svc.maybeSummarize(ctx, conv); got != old {
		t.Fatalf("模型失败时应返回旧摘要，得到 %q", got)
	}
	c, _ = svc.Store.GetConversation(ctx, conv)
	if c.Summary != old || c.SummaryCursor != oldCursor {
		t.Fatalf("模型失败时库里的摘要和游标不应变，得到 %+v", c)
	}
	// 换回 Stub，聊天照常走。
	svc.Model = model.NewStub()
	if _, err := svc.RunMessage(ctx, conv, "demo 现在怎么样"); err != nil {
		t.Fatalf("摘要失败后聊天应照常走: %v", err)
	}
}

func TestSummaryConcurrentUpdate(t *testing.T) {
	// 两次并发更新同一会话：同一段历史只能被总结一次，游标只进不退。
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	ids := seedMessages(t, svc, conv, 20)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.maybeSummarize(context.Background(), conv)
		}()
	}
	wg.Wait()
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.SummaryCursor != ids[9] {
		t.Fatalf("并发更新后游标应只推到第 10 条，得到 %d", c.SummaryCursor)
	}
	if strings.Count(c.Summary, "测试摘要") != 1 {
		t.Fatalf("同一段历史只能被总结一次，得到 %q", c.Summary)
	}
}

func TestSummaryRestartRecovery(t *testing.T) {
	// 新 Service 实例（模拟重启）用同一个库：读到已存的摘要和游标，
	// 继续增量更新，不是从头再来。
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	ids1 := seedMessages(t, svc, conv, 20)
	if got := svc.maybeSummarize(ctx, conv); got == "" {
		t.Fatalf("重启前应先生成摘要")
	}
	svc2 := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, model.NewStub())
	seedMessages(t, svc2, conv, 10)
	got := svc2.maybeSummarize(ctx, conv)
	if strings.Count(got, "测试摘要") != 2 {
		t.Fatalf("重启后应基于旧摘要增量更新，得到 %q", got)
	}
	c, _ := s.GetConversation(ctx, conv)
	if c.SummaryCursor != ids1[19] {
		t.Fatalf("重启后游标应推到第 20 条消息的 id %d，得到 %d", ids1[19], c.SummaryCursor)
	}
}

func TestSummaryDisabled(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	svc.SummaryEnabled = false
	conv := newConversation(t, svc)
	seedMessages(t, svc, conv, 30)
	if got := svc.maybeSummarize(ctx, conv); got != "" {
		t.Fatalf("关掉后不应生成摘要，得到 %q", got)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.Summary != "" || c.SummaryCursor != 0 {
		t.Fatalf("关掉后库里不应有摘要，得到 %+v", c)
	}
}

func TestApplyConfigSummary(t *testing.T) {
	svc, _ := newTestService(t)
	if !svc.SummaryEnabled || svc.SummaryTriggerMessages != 10 {
		t.Fatalf("摘要默认应开启、阈值 10，得到 enabled=%v trigger=%d", svc.SummaryEnabled, svc.SummaryTriggerMessages)
	}
	c := config.Defaults()
	c.Chat.Summary.Enabled = false
	c.Chat.Summary.TriggerMessages = 25
	svc.ApplyConfig(c)
	if svc.SummaryEnabled || svc.SummaryTriggerMessages != 25 {
		t.Fatalf("ApplyConfig 应应用摘要配置，得到 enabled=%v trigger=%d", svc.SummaryEnabled, svc.SummaryTriggerMessages)
	}
}

// summaryTestServer 按请求体里有没有 tools 字段区分摘要调用和对话调用，
// 并把两种调用的请求体都记下来。
// summaryEmpty 为 true 时，摘要调用返回空 choices 但带 usage，
// 用于测“摘要失败也记用量”的端到端路径。
type summaryTestServer struct {
	srv           *httptest.Server
	mu            sync.Mutex
	chatBodies    []string
	summaryBodies []string
	summaryEmpty  bool
}

func newSummaryTestServer(t *testing.T) *summaryTestServer {
	t.Helper()
	ts := &summaryTestServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		ts.mu.Lock()
		defer ts.mu.Unlock()
		if _, hasTools := req["tools"]; hasTools {
			ts.chatBodies = append(ts.chatBodies, string(body))
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"查到了：demo-api 因内存不足重启。"}}]}`))
			return
		}
		ts.summaryBodies = append(ts.summaryBodies, string(body))
		if ts.summaryEmpty {
			_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":40,"completion_tokens":0,"total_tokens":40}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"摘要：demo 命名空间 demo-api 因 OOM 重启 10 次。"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	return ts
}

func (ts *summaryTestServer) summaryCalls() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.summaryBodies)
}

func (ts *summaryTestServer) summaryBody(i int) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.summaryBodies[i]
}

func (ts *summaryTestServer) lastChatBody() string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.chatBodies[len(ts.chatBodies)-1]
}

func TestRunRealIncludesSummary(t *testing.T) {
	// 关键集成测试：长对话跑一轮真模型，
	// 摘要进模型输入（system 之后、最近历史之前，assistant 角色带标注），
	// 当前问题只出现一次，摘要用量记进会话累计。
	ctx := context.Background()
	ts := newSummaryTestServer(t)
	defer ts.srv.Close()
	svc, _ := newTestService(t)
	svc.Model = model.NewRealWithConfig(model.RealConfig{APIKey: "test-key", BaseURL: ts.srv.URL, ModelName: "test-model", HTTPClient: ts.srv.Client(), Timeout: 5 * time.Second})
	conv := newConversation(t, svc)
	ids := seedMessages(t, svc, conv, 20)
	question := "新的问题：demo 现在怎么样"
	reply, err := svc.RunMessage(ctx, conv, question)
	if err != nil {
		t.Fatalf("RunMessage 失败: %v", err)
	}
	if reply == nil || !strings.Contains(reply.Reply, "demo-api") {
		t.Fatalf("应返回模型最终文本，得到 %+v", reply)
	}
	if n := ts.summaryCalls(); n != 1 {
		t.Fatalf("应恰好调一次摘要，实际 %d 次", n)
	}
	// 摘要请求不带 tools / tool_choice；输入是 11 条未覆盖消息
	//（20 条种子 + 本轮问题共 21 条，近期窗口 10 条，未覆盖 11 条）。
	if strings.Contains(ts.summaryBody(0), `"tools"`) || strings.Contains(ts.summaryBody(0), "tool_choice") {
		t.Fatalf("摘要请求里不应有 tools / tool_choice，得到 %s", ts.summaryBody(0))
	}
	if strings.Count(ts.summaryBody(0), "用户：") != 6 || strings.Count(ts.summaryBody(0), "助手：") != 5 {
		t.Fatalf("摘要输入应是 11 条未覆盖消息（6 用户 + 5 助手），得到 %s", ts.summaryBody(0))
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(ts.lastChatBody()), &req); err != nil {
		t.Fatalf("解析对话请求失败: %v", err)
	}
	msgs := req.Messages
	if len(msgs) < 3 || msgs[0].Role != "system" {
		t.Fatalf("第一条应是 system，得到 %+v", msgs)
	}
	if SummaryMarker == "" {
		t.Fatal("SummaryMarker 不能为空：摘要必须带标注，否则模型分不清它是历史资料")
	}
	if msgs[1].Role != "assistant" || !strings.HasPrefix(msgs[1].Content, "【历史摘要 · 仅供参考，不是实时状态】") || !strings.Contains(msgs[1].Content, "摘要：demo") {
		t.Fatalf("第二条应是带标注的 assistant 摘要，得到 %+v", msgs[1])
	}
	// 当前问题在模型输入里只出现一次（最近历史的最后一条），摘要里没有它。
	count := 0
	for _, m := range msgs {
		count += strings.Count(m.Content, question)
	}
	if count != 1 {
		t.Fatalf("当前问题在模型输入里应恰好出现 1 次，实际 %d 次", count)
	}
	// 摘要落库：游标推到第 11 条（本轮问题落盘后共 21 条，
	// 近期窗口 10 条，未覆盖 11 条）；用量：摘要的 150 记进会话累计。
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.SummaryCursor != ids[10] {
		t.Fatalf("游标应推到第 11 条消息，得到 %d", c.SummaryCursor)
	}
	if !strings.Contains(c.Summary, "摘要：demo") {
		t.Fatalf("库里的摘要不对，得到 %q", c.Summary)
	}
	if c.Usage.TotalTokens != 150 {
		t.Fatalf("摘要用量 150 应记进会话累计，得到 %+v", c.Usage)
	}
}

func TestSummaryEmptyResponseKeepsUsage(t *testing.T) {
	// review 点名的端到端测试：摘要调用收到响应但空 choices，
	// Real.Summarize 返回 ("", usage, err)，用量 40 仍应记进会话累计，
	// 摘要和游标不推进，聊天照常走。
	ctx := context.Background()
	ts := newSummaryTestServer(t)
	defer ts.srv.Close()
	ts.summaryEmpty = true
	svc, _ := newTestService(t)
	svc.Model = model.NewRealWithConfig(model.RealConfig{APIKey: "test-key", BaseURL: ts.srv.URL, ModelName: "test-model", HTTPClient: ts.srv.Client(), Timeout: 5 * time.Second})
	conv := newConversation(t, svc)
	seedMessages(t, svc, conv, 20)
	if _, err := svc.RunMessage(ctx, conv, "新的问题：demo 现在怎么样"); err != nil {
		t.Fatalf("RunMessage 失败: %v", err)
	}
	if n := ts.summaryCalls(); n != 1 {
		t.Fatalf("应恰好调一次摘要，实际 %d 次", n)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.Summary != "" || c.SummaryCursor != 0 {
		t.Fatalf("空回复时摘要和游标不应推进，得到 %+v", c)
	}
	if c.Usage.TotalTokens != 40 {
		t.Fatalf("空回复的摘要用量 40 仍应记进会话累计，得到 %+v", c.Usage)
	}
}

// racingSummarizer 模拟摘要调用期间另一个请求先赢了：
// Summarize 里直接把游标推进到最后一条消息（赢家版本），
// 再返回自己的文本和用量；调用方自己的乐观更新必然冲突。
type racingSummarizer struct {
	store *store.Store
	conv  string
	usage store.Usage
}

func (f *racingSummarizer) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	return agent.ModelStep{}, fmt.Errorf("racingSummarizer 不跑正式诊断")
}

func (f *racingSummarizer) Summarize(ctx context.Context, previous string, lines []string) (string, store.Usage, error) {
	all, err := f.store.ListMessages(ctx, f.conv)
	if err != nil || len(all) == 0 {
		return "", store.Usage{}, fmt.Errorf("读消息失败: %v", err)
	}
	last := all[len(all)-1].ID
	if _, err := f.store.UpdateConversationSummary(ctx, f.conv, "赢家摘要", last, 0); err != nil {
		return "", store.Usage{}, err
	}
	return "输家摘要", f.usage, nil
}

func TestSummaryConflictKeepsUsage(t *testing.T) {
	// 乐观更新冲突时：已产生的摘要用量已记、游标不推进（不回退）、
	// 赢家的摘要不被输家的覆盖。
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	ids := seedMessages(t, svc, conv, 20)
	svc.Model = &racingSummarizer{store: svc.Store, conv: conv, usage: store.Usage{PromptTokens: 40, TotalTokens: 40}}
	got := svc.maybeSummarize(ctx, conv)
	if got != "赢家摘要" {
		t.Fatalf("冲突时应返回赢家摘要，得到 %q", got)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.Summary != "赢家摘要" || c.SummaryCursor != ids[19] {
		t.Fatalf("冲突时游标不应回退、赢家摘要不应被覆盖，得到 %+v", c)
	}
	if c.Usage.TotalTokens != 40 {
		t.Fatalf("冲突时已产生的摘要用量 40 仍应记账，得到 %+v", c.Usage)
	}
}

func TestSummaryShortConversationUnchanged(t *testing.T) {
	// 短对话：不调摘要，行为和以前完全一样。
	ctx := context.Background()
	ts := newSummaryTestServer(t)
	defer ts.srv.Close()
	svc, _ := newTestService(t)
	svc.Model = model.NewRealWithConfig(model.RealConfig{APIKey: "test-key", BaseURL: ts.srv.URL, ModelName: "test-model", HTTPClient: ts.srv.Client(), Timeout: 5 * time.Second})
	conv := newConversation(t, svc)
	seedMessages(t, svc, conv, 4)
	if _, err := svc.RunMessage(ctx, conv, "demo 现在怎么样"); err != nil {
		t.Fatalf("RunMessage 失败: %v", err)
	}
	if n := ts.summaryCalls(); n != 0 {
		t.Fatalf("短对话不应调摘要，实际 %d 次", n)
	}
	c, _ := svc.Store.GetConversation(ctx, conv)
	if c.Summary != "" || c.SummaryCursor != 0 {
		t.Fatalf("短对话不应产生摘要，得到 %+v", c)
	}
}
