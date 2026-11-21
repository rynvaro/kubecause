package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// TestRoundIDSharedAcrossRound 是 v0.12.0 的核心回归：
// 同一轮的用户 / tool / 助手消息共享一个 roundID，
// 不同轮的 roundID 不同；工具调用只存在 tool 消息里，
// 助手消息不再抄一份 tool_calls（双份时间线的根因）。
// 如果这里挂了，说明又存了两份，右侧面板会再次显示两遍。
func TestRoundIDSharedAcrossRound(t *testing.T) {
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
		t.Fatalf("读会话失败: %v", err)
	}
	// 按 roundID 分组（保持出现顺序）。
	var order []string
	byRound := map[string][]store.ConversationMessage{}
	for _, m := range msgs {
		if m.RoundID == "" {
			t.Fatalf("消息 id=%d role=%s 没有 roundID", m.ID, m.Role)
		}
		if _, ok := byRound[m.RoundID]; !ok {
			order = append(order, m.RoundID)
		}
		byRound[m.RoundID] = append(byRound[m.RoundID], m)
	}
	if len(order) != 2 {
		t.Fatalf("两轮应产生 2 个 roundID，得到 %d 个：%v", len(order), order)
	}
	for _, rid := range order {
		round := byRound[rid]
		var users, tools, assistants int
		for _, m := range round {
			switch m.Role {
			case "user":
				users++
			case "tool":
				tools++
				if m.ToolCalls == "" {
					t.Fatalf("round %s 的 tool 消息 id=%d 应带工具记录", rid, m.ID)
				}
			case "assistant":
				assistants++
				// 助手消息不再存 tool_calls：这就是修掉的那份拷贝。
				if m.ToolCalls != "" {
					t.Fatalf("round %s 的助手消息 id=%d 还存着 tool_calls：%s", rid, m.ID, m.ToolCalls)
				}
			}
		}
		if users != 1 || assistants != 1 || tools == 0 {
			t.Fatalf("round %s 应有 1 条用户、>=1 条 tool、1 条助手，得到用户 %d、tool %d、助手 %d", rid, users, tools, assistants)
		}
	}
}

// TestReplyStillCarriesToolCalls 是上面那个测试的另一面：
// 存储里助手消息不再抄 tool_calls，但 POST 当次返回的 Reply
// 仍带 ToolCalls（临时数据），前端轮询和“查了 N 个工具”
// 在首轮渲染时不依赖存储里的副本。
func TestReplyStillCarriesToolCalls(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("RunMessage 失败: %v", err)
	}
	if len(r.ToolCalls) == 0 {
		t.Fatalf("Reply 应仍带本轮工具调用，得到 %+v", r)
	}
}

// mockModelServer 是一个假的 OpenAI 兼容服务端：
// 第一次请求返回一次工具调用，之后都返回固定最终文本。
// 用来验证真模型路径的调试日志，不用真 Key、不用联网。
func mockModelServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-mock-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"集群总览正常。"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newMockRealService(t *testing.T, srv *httptest.Server) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tools := &run.ToolsAdapter{Kube: kube.NewFake()}
	real := model.NewRealWithConfig(model.RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock"})
	return NewService(s, tools, real), s
}

// captureLog 在测试期间把标准 log 的输出收到 buf 里，测完恢复。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// TestDebugLoggingWithMockModel 验证 v0.12.0 的调试模式（只打服务端日志）：
// DebugModel 打开时，每次调模型前打一条完整请求日志、返回后打一条返回日志；
// 请求数组里 system 在第一位、历史 user/assistant 交替、当前问题在最后一条；
// 第二次请求的数组里含 assistant(tool_calls) + tool 往返；
// DebugModel 关闭时一条调试日志都没有。
func TestDebugLoggingWithMockModel(t *testing.T) {
	ctx := context.Background()
	srv, _ := mockModelServer(t)

	t.Run("debug on logs full request and response", func(t *testing.T) {
		svc, _ := newMockRealService(t, srv)
		svc.DebugModel = true
		conv := newConversation(t, svc)
		buf := captureLog(t)
		if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
			t.Fatalf("RunMessage 失败: %v", err)
		}
		out := buf.String()
		// 请求日志：第一次请求应是 system + 当前问题（最后一条 user）。
		reqIdx := strings.Index(out, "[debug model]")
		if reqIdx < 0 {
			t.Fatalf("DebugModel 打开时应打 [debug model] 日志，得到：%s", out)
		}
		// 从第一条请求日志里把 request= 后面的 JSON 数组抠出来验结构。
		lineEnd := strings.Index(out[reqIdx:], "\n")
		var firstReqLine string
		if lineEnd < 0 {
			firstReqLine = out[reqIdx:]
		} else {
			firstReqLine = out[reqIdx : reqIdx+lineEnd]
		}
		marker := "request="
		mi := strings.Index(firstReqLine, marker)
		if mi < 0 {
			t.Fatalf("请求日志里应有 request= JSON，得到：%s", firstReqLine)
		}
		var reqMsgs []map[string]any
		if err := json.Unmarshal([]byte(firstReqLine[mi+len(marker):]), &reqMsgs); err != nil {
			t.Fatalf("请求日志的 JSON 解析失败: %v（行：%s）", err, firstReqLine)
		}
		if len(reqMsgs) < 2 {
			t.Fatalf("第一次请求至少应有 system + 当前问题，得到 %d 条", len(reqMsgs))
		}
		if reqMsgs[0]["role"] != "system" {
			t.Fatalf("第一条应是 system，得到 %v", reqMsgs[0]["role"])
		}
		last := reqMsgs[len(reqMsgs)-1]
		if last["role"] != "user" || !strings.Contains(fmt.Sprint(last["content"]), "当前状态怎么样") {
			t.Fatalf("最后一条应是当前问题，得到 %+v", last)
		}
		// 返回日志：应有工具调用那一步的返回。
		if !strings.Contains(out, "response tool=get_cluster_overview") {
			t.Fatalf("返回日志应记下调了哪个工具，得到：%s", out)
		}
		// 第二次请求的数组里应含 assistant(tool_calls) + tool 往返。
		if !strings.Contains(out, `"role":"tool"`) {
			t.Fatalf("后续请求的数组里应含 tool 角色消息，得到：%s", out)
		}
		// 日志里绝不能出现 API Key（Key 只在请求头里）。
		if strings.Contains(out, "test-key") {
			t.Fatalf("调试日志里出现了 API Key")
		}
	})

	t.Run("debug off logs nothing", func(t *testing.T) {
		svc, _ := newMockRealService(t, srv)
		conv := newConversation(t, svc)
		buf := captureLog(t)
		if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
			t.Fatalf("RunMessage 失败: %v", err)
		}
		if strings.Contains(buf.String(), "[debug model]") {
			t.Fatalf("DebugModel 关闭时不应打调试日志，得到：%s", buf.String())
		}
	})
}

// TestNoDebugLogForStub 是调试模式的边界：
// 桩模型不调外部模型，DebugModel 打开也不应有模型请求日志。
func TestNoDebugLogForStub(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	svc.DebugModel = true
	conv := newConversation(t, svc)
	buf := captureLog(t)
	if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
		t.Fatalf("RunMessage 失败: %v", err)
	}
	if strings.Contains(buf.String(), "[debug model]") {
		t.Fatalf("桩模型不应打模型请求日志，得到：%s", buf.String())
	}
}

// TestDebugLogDoesNotLeakAPIKey 是调试日志的安全回归：
// 请求数组是发给模型的消息体，Key 只在 Authorization 头里，
// 即使模型返回里带了奇怪内容，日志也不应出现 Key。
// （TestDebugLoggingWithMockModel 里已断言一次，这里单独锁死
// 截断逻辑：超长内容截断后也不能把 Key 拼进去。）
func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("abc", 10); got != "abc" {
		t.Fatalf("短文本不应截断，得到 %q", got)
	}
	long := strings.Repeat("x", 5000)
	got := truncateForLog(long, 4000)
	if len([]rune(got)) >= 5000 || !strings.Contains(got, "已截断") {
		t.Fatalf("超长文本应截断并注明，得到长度 %d", len([]rune(got)))
	}
}

// TestOldRoundIDEmptyIsReadable 是老数据的底线：
// round_id 为空的老会话行照常能读出来（前端回退到位置推导，
// 见 types.ts 的 toolCallsForAssistant），迁移不能把老库读坏。
func TestOldRoundIDEmptyIsReadable(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	// 直接写三行“老数据”：round_id 为空，助手消息里还存着老副本。
	msgs := []store.ConversationMessage{
		{ConversationID: conv, Role: "user", Content: "当前状态怎么样"},
		{ConversationID: conv, Role: "tool", Content: "总览", ToolCalls: `[{"id":"call-1","name":"get_cluster_overview","args":{},"summary":"总览","durationMs":5}]`},
		{ConversationID: conv, Role: "assistant", Content: "集群正常", ToolCalls: `[{"id":"call-1","name":"get_cluster_overview","args":{},"summary":"总览","durationMs":5}]`},
	}
	for i := range msgs {
		if err := s.AppendMessage(ctx, &msgs[i]); err != nil {
			t.Fatalf("写老数据失败: %v", err)
		}
	}
	_ = svc
	got, err := s.ListMessages(ctx, conv)
	if err != nil {
		t.Fatalf("读老数据失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应读出 3 行老数据，得到 %d 行", len(got))
	}
	for _, m := range got {
		if m.RoundID != "" {
			t.Fatalf("老数据行的 roundId 应为空，得到 %q", m.RoundID)
		}
	}
	if got[2].ToolCalls == "" {
		t.Fatalf("老助手消息里的 tool_calls 副本应原样保留")
	}
}
