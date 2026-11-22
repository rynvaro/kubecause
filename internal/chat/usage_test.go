package chat

import (
	"context"
	"sync/atomic"
	"testing"

	"kubecause/internal/store"
)

func TestChatAccumulatesUsageIntoConversation(t *testing.T) {
	// 对话：两轮 Chat 的用量都要累加进会话，
	// 第一轮工具调用和第二轮最终文本的用量都算。
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下总览。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"总览正常，没有异常 Pod。"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, s := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if r == nil || r.Reply == "" {
		t.Fatalf("对话应返回回复，得到 %+v", r)
	}
	if got := *calls; got != 2 {
		t.Fatalf("应调 2 次模型，得到 %d 次", got)
	}
	c, err := s.GetConversation(ctx, conv)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if want := (store.Usage{PromptTokens: 110, CompletionTokens: 55, TotalTokens: 165}); c.Usage != want {
		t.Fatalf("两轮用量应累加为 %+v，得到 %+v", want, c.Usage)
	}
}

func TestChatZeroUsageNotPersisted(t *testing.T) {
	// 响应里没带 usage 时会话用量保持零值，不编造、不写库。
	ctx := context.Background()
	resp := `{"choices":[{"message":{"role":"assistant","content":"总览正常。"}}]}`
	srv, _, _ := scriptedModelServer(t, []string{resp})
	svc, s := newRealService(t, srv)
	conv := newConversation(t, svc)
	if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	c, err := s.GetConversation(ctx, conv)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if !c.Usage.IsZero() {
		t.Fatalf("没带 usage 的响应应用量应为零值，得到 %+v", c.Usage)
	}
}

func TestChatCorrectionKeepsFirstCallUsage(t *testing.T) {
	// 审查第 1 项的点名测试：第一次交报告时 facts 是数字（格式错误），
	// 纠错一次后模型重交正确报告；两次调用的用量都要记进会话，
	// 不能只记纠错那一次、把第一次烧掉的 token 丢了。
	ctx := context.Background()
	bad := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`
	good := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"demo-api 重启 5 次\"],\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`
	srv, _, calls := scriptedModelServer(t, []string{bad, good})
	svc, s := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么一直重启")
	if err != nil {
		t.Fatalf("纠错后对话不应失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("纠错应只多发 1 次请求，总共 2 次，得到 %d 次", got)
	}
	if r.Structured["report"] == nil {
		t.Fatalf("重交后 Structured 里应有 report")
	}
	c, err := s.GetConversation(ctx, conv)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if want := (store.Usage{PromptTokens: 120, CompletionTokens: 60, TotalTokens: 180}); c.Usage != want {
		t.Fatalf("两次调用的用量应累加为 %+v，得到 %+v", want, c.Usage)
	}
}
