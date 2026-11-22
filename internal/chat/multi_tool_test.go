package chat

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatExecutesMultipleToolCallsInOneRound(t *testing.T) {
	// 回归：模型一次返回多个工具调用时，对话循环一轮里按顺序全执行，
	// 两个结果都回传给模型、都进 Structured，不能只执行第一个。
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下总览和有问题的 Pod。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}},{"id":"call-2","type":"function","function":{"name":"list_problem_pods","arguments":"{\"namespace\":\"demo\"}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"都查到了，demo 有 1 个有问题的 Pod。"}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("一批 2 个调用 + 最终文本应只发 2 次请求，得到 %d 次", got)
	}
	if len(r.ToolCalls) != 2 {
		t.Fatalf("回复应带 2 条工具调用记录，得到 %d 条", len(r.ToolCalls))
	}
	if r.ToolCalls[0].Name != "get_cluster_overview" || r.ToolCalls[1].Name != "list_problem_pods" {
		t.Fatalf("两个调用应按返回顺序执行，得到 %v", r.ToolCalls)
	}
	if r.Structured["overview"] == nil || r.Structured["problemPods"] == nil {
		t.Fatalf("两个调用的结果都应进 Structured，得到 %v", r.Structured)
	}
	if !strings.Contains(r.Reply, "我先看一下总览和有问题的 Pod。") || !strings.Contains(r.Reply, "都查到了") {
		t.Fatalf("顺带的文字和最终文本都应在回复里，得到 %q", r.Reply)
	}
}

func TestChatSubmitDiagnosisSkipsOtherCallsInBatch(t *testing.T) {
	// submit_diagnosis 是终结信号：同一批里只要出现它，只处理它，
	// 同批其他查询调用不执行，本轮直接结束——不管它在第几个位置。
	ctx := context.Background()
	resp := `{"choices":[{"message":{"role":"assistant","content":"查到了。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}},{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"demo-api 重启 5 次\"],\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"medium\",\"recommendations\":[\"调高内存 limit\"]}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么一直重启")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("一批里有 submit_diagnosis 时应只发 1 次请求，得到 %d 次", got)
	}
	if len(r.ToolCalls) != 0 {
		t.Fatalf("同批查询调用应跳过、不执行，得到 %+v", r.ToolCalls)
	}
	if r.Structured["report"] == nil {
		t.Fatalf("Structured 里应有 report")
	}
	if !strings.Contains(r.Reply, "诊断结论：容器内存限制过低") {
		t.Fatalf("回复里应有格式化报告，得到 %q", r.Reply)
	}
}
