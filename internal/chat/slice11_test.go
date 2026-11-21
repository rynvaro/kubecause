package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// seedRounds 往会话里写几轮历史，每轮 1 条用户 + 6 条 tool + 1 条助手，
// 和真跑起来的落盘形状一致（recordTool 每调一次工具单独存一条 tool 消息）。
// 每轮的用户问题用 i 标记，方便断言最早那轮还在不在。
func seedRounds(t *testing.T, s *store.Store, conv string, rounds int) {
	t.Helper()
	ctx := context.Background()
	for i := 1; i <= rounds; i++ {
		_ = s.AppendMessage(ctx, &store.ConversationMessage{
			ConversationID: conv, Role: "user",
			Content: fmt.Sprintf("第%d轮：demo-api 为什么一直重启", i),
		})
		for j := 1; j <= 6; j++ {
			_ = s.AppendMessage(ctx, &store.ConversationMessage{
				ConversationID: conv, Role: "tool",
				Content: fmt.Sprintf("第%d轮工具%d：get_pod demo-api", i, j),
				ToolCalls: fmt.Sprintf(`[{"id":"r%d-t%d","name":"get_pod","args":{"namespace":"demo"}}]`, i, j),
			})
		}
		_ = s.AppendMessage(ctx, &store.ConversationMessage{
			ConversationID: conv, Role: "assistant",
			Content: fmt.Sprintf("第%d轮回答：demo-api 内存 limit 太低。", i),
		})
	}
}

// TestHistoryCountsOnlyUserAndAssistant 覆盖“历史只算 user/assistant 消息”：
// maxHistoryMessages=10 时，按旧口径（全量行数）3 轮历史（24 行）
// 只能覆盖约 1 轮，第一轮的用户问题会被截掉；按新口径
// （只算 user 和 assistant）3 轮只有 6 条，全保留，
// 第一轮的问题仍在模型上下文里。6 轮时（12 条用户/助手）
// 截到 10 条，第一轮被截掉、第二轮还在，且 tool 消息一条都不占名额。
func TestHistoryCountsOnlyUserAndAssistant(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)

	seedRounds(t, s, conv, 3)
	got, err := svc.lastMessages(ctx, conv, svc.MaxHistoryMessages)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("3 轮历史应保留 6 条用户/助手消息，得到 %d 条", len(got))
	}
	for _, m := range got {
		if m.Role == "tool" {
			t.Fatalf("历史里不应有 tool 消息，得到 %+v", m)
		}
	}
	joined := ""
	for _, m := range got {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "第1轮：demo-api 为什么一直重启") {
		t.Fatalf("3 轮时第一轮的用户问题应在历史里，得到 %q", joined)
	}

	conv2 := newConversation(t, svc)
	seedRounds(t, s, conv2, 6)
	got2, err := svc.lastMessages(ctx, conv2, svc.MaxHistoryMessages)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(got2) != 10 {
		t.Fatalf("6 轮历史应截断到 10 条用户/助手消息，得到 %d 条", len(got2))
	}
	joined2 := ""
	for _, m := range got2 {
		if m.Role == "tool" {
			t.Fatalf("历史里不应有 tool 消息，得到 %+v", m)
		}
		joined2 += m.Content + "\n"
	}
	if strings.Contains(joined2, "第1轮：") {
		t.Fatalf("6 轮时第一轮应已被截掉，得到 %q", joined2)
	}
	if !strings.Contains(joined2, "第2轮：demo-api 为什么一直重启") {
		t.Fatalf("6 轮时第二轮的用户问题应在历史里，得到 %q", joined2)
	}
}

// capturedRequest 是 mock 模型收到的一次 OpenAI 兼容请求里
// 发给模型的消息列表，只取 role 和 content 做断言。
type capturedRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// newCapturingReal 搭一个 mock 真模型：每次都返回一段纯文本最终回答，
// 不调工具、不提交报告；每次收到的请求体都记下来，
// 测试用它确认发给模型的消息里到底有什么、没有什么。
func newCapturingReal(t *testing.T) (*model.Real, *[]capturedRequest) {
	t.Helper()
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req capturedRequest
		_ = json.Unmarshal(body, &req)
		captured = append(captured, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"初步判断是内存问题。"}}]}`)
	}))
	t.Cleanup(srv.Close)
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	return real, &captured
}

func countUserMessagesWithContent(req capturedRequest, content string) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content == content {
			n++
		}
	}
	return n
}

// TestRealHistoryKeepsFirstRoundAndQuestionSentOnce 覆盖两个修法：
//  1. 历史只算 user/assistant：先写 3 轮含 tool 消息的历史，
//     再问一个新问题，mock 模型第一次收到的请求里仍应有
//     第一轮的用户问题（旧口径下它会被 tool 消息挤掉）；
//     上一轮的 tool 消息不应再以“工具返回：”的形式出现在请求里。
//  2. 当前问题只出现一次：RunMessage 先把用户消息落盘，
//     history 里已经有这句了，runReal 不能再追加一遍。
//     mock 模型每次只给纯文本；以前这里会触发一次追问结构化建议，
//     一共收到 2 次请求。v0.13.0 删掉了追问，只收到 1 次请求，
//     当前问题在其中恰好出现一次，请求里也不再有追问那句假 user 消息。
func TestRealHistoryKeepsFirstRoundAndQuestionSentOnce(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real, captured := newCapturingReal(t)
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	conv := newConversation(t, svc)
	seedRounds(t, s, conv, 3)

	question := "那个一直重启的现在怎么样了"
	if _, err := svc.RunMessage(ctx, conv, question); err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("mock 模型应只收到 1 次请求（纯文本直接返回，不再追问结构化建议），得到 %d 次", len(*captured))
	}
	first := (*captured)[0]
	joined := ""
	for _, m := range first.Messages {
		joined += m.Role + "：" + m.Content + "\n"
	}
	if strings.Contains(joined, "如果你认为需要修复") {
		t.Fatalf("请求里不应再有追问结构化建议的假 user 消息，得到:\n%s", joined)
	}
	if !strings.Contains(joined, "第1轮：demo-api 为什么一直重启") {
		t.Fatalf("第一次请求里应仍有第一轮的用户问题，得到:\n%s", joined)
	}
	if strings.Contains(joined, "工具返回：") {
		t.Fatalf("上一轮的 tool 消息不应再以“工具返回：”进模型上下文，得到:\n%s", joined)
	}
	if n := countUserMessagesWithContent(first, question); n != 1 {
		t.Fatalf("第一次请求里当前问题应恰好出现一次，出现 %d 次，请求内容:\n%s", n, joined)
	}
}
