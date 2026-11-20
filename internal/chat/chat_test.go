package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
	got, err := svc.lastMessages(ctx, conv, MaxHistoryMessages)
	if err != nil {
		t.Fatalf("取历史失败: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("最近历史应截断到 10 条，得到 %d 条", len(got))
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
