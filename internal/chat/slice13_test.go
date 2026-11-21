package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kubecause/internal/kube"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// scriptedModelServer 按调用次数顺序返回写好的响应体，并把每次的请求体原样记下来。
// responses[i] 是第 i+1 次调用的返回；调用次数超过表长时返回最后一项。
// 用来验证真模型路径在固定剧本下的行为，不用真 Key、不用联网。
func scriptedModelServer(t *testing.T, responses []string) (*httptest.Server, *[]string, *int32) {
	t.Helper()
	var calls int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		idx := int(n) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		fmt.Fprint(w, responses[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &calls
}

// TestTextOnlyFinalReturnsDirectly 是 v0.13.0 拿掉追问的核心回归：
// 模型只给一段最终文本、不交结构化报告时，直接把这段文本当回复返回，
// 只发 1 次请求，不再追加假 user 消息追问一次。
// 以前这里会多发一次请求，追问那句一出现，简单问题也会被推成诊断报告。
func TestTextOnlyFinalReturnsDirectly(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real, captured := newCapturingReal(t)
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo中有几个pod")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("只给文字不给报告时应只发 1 次请求（不再追问），得到 %d 次", len(*captured))
	}
	if r.Reply != "初步判断是内存问题。" {
		t.Fatalf("回复应就是模型那段文字，得到 %q", r.Reply)
	}
	for _, m := range (*captured)[0].Messages {
		if strings.Contains(m.Content, "如果你认为需要修复") {
			t.Fatalf("请求里不应再有追问结构化建议的假 user 消息")
		}
	}
	if r.SuggestedRepair != nil || r.SuggestedCreation != nil {
		t.Fatalf("没交报告时不应有建议卡，得到 %+v", r)
	}
}

// TestTextAndReportBothKept 覆盖模型一次返回里文字和报告同时存在：
// 回复里文字在前、格式化报告在后，两边都在，谁也不替换谁。
// 以前 formatReportReply 会把模型文字整个替换掉，只剩报告。
func TestTextAndReportBothKept(t *testing.T) {
	ctx := context.Background()
	resp := `{"choices":[{"message":{"role":"assistant","content":"查到了，demo-api 只有 1 个 Pod，正在重启。","tool_calls":[{"id":"call-diag-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"demo-api 重启 5 次\"],\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"medium\",\"recommendations\":[\"调高内存 limit\"]}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么一直重启")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("应只发 1 次请求，得到 %d 次", got)
	}
	text := "查到了，demo-api 只有 1 个 Pod，正在重启。"
	if !strings.Contains(r.Reply, text) {
		t.Fatalf("回复里应保留模型文字，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "诊断结论：容器内存限制过低") {
		t.Fatalf("回复里应有格式化报告，得到 %q", r.Reply)
	}
	if strings.Index(r.Reply, text) > strings.Index(r.Reply, "诊断结论") {
		t.Fatalf("模型文字应在报告前面，得到 %q", r.Reply)
	}
	if r.Structured["report"] == nil {
		t.Fatalf("Structured 里应有 report")
	}
}

// TestToolCallSideTextKept 覆盖模型调工具时顺带的文字：
// 第一次返回一边说“我先看一下集群总览。”一边调 get_cluster_overview，
// 这段文字要拼进最终回复（按步骤顺序在最终文本前面），
// 第二次请求的数组里那条 assistant 消息也要同时带文字和 tool_calls，
// 而不是像以前那样 Content 写死成空、文字直接丢了。
func TestToolCallSideTextKept(t *testing.T) {
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下集群总览。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"集群总览正常。"}}]}`
	srv, bodies, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("应发 2 次请求（工具调用 + 最终文本），得到 %d 次", got)
	}
	if !strings.Contains(r.Reply, "我先看一下集群总览。") {
		t.Fatalf("调工具时顺带的文字应拼进最终回复，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "集群总览正常。") {
		t.Fatalf("最终文本应在回复里，得到 %q", r.Reply)
	}
	if strings.Index(r.Reply, "我先看一下集群总览。") > strings.Index(r.Reply, "集群总览正常。") {
		t.Fatalf("顺带的文字应按步骤顺序在最终文本前面，得到 %q", r.Reply)
	}
	if len(*bodies) < 2 {
		t.Fatalf("应记下 2 次请求体，得到 %d 次", len(*bodies))
	}
	second := (*bodies)[1]
	if !strings.Contains(second, "我先看一下集群总览。") || !strings.Contains(second, "get_cluster_overview") {
		t.Fatalf("第二次请求的 assistant 消息应同时带文字和 tool_calls，得到：%s", second)
	}
}
