package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"kubecause/internal/kube"
)

// podRowsFromStructured 把 Structured 里某份 Pod 数据解析成行对象，
// 行里必须带 name 等字段——字符串数组在这里解析失败，
// 测试就是要抓住空表那种形状回归。
func podRowsFromStructured(t *testing.T, v any) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	var out struct {
		Pods []map[string]any `json:"pods"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("structured 里的 pods 应是行对象数组，解析失败: %v", err)
	}
	return out.Pods
}

func toolCalled(r *Reply, name string) bool {
	for _, tc := range r.ToolCalls {
		if tc.Name == name {
			return true
		}
	}
	return false
}

// TestStubListPodsQuestion 覆盖桩模型路径的“列全部 Pod”分支：
// 用户问“demo 命名空间下有哪些 Pod”，桩模型直接调 list_pods，
// 不用先猜 Deployment，也不走“找问题 Pod 再深挖”的诊断流程。
func TestStubListPodsQuestion(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)

	r, err := svc.RunMessage(ctx, conv, "demo 命名空间下有哪些 Pod")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if !toolCalled(r, "list_pods") {
		t.Fatalf("问全部 Pod 时应调 list_pods，调了 %+v", r.ToolCalls)
	}
	rows := podRowsFromStructured(t, r.Structured["pods"])
	if len(rows) != 1 || rows[0]["name"] != kube.DemoPod {
		t.Fatalf("structured[\"pods\"] 应是 demo 下全部 Pod 的完整行，得到 %+v", rows)
	}
	if rows[0]["ready"] != "1/1" || rows[0]["restarts"] != float64(5) {
		t.Fatalf("行里应带就绪和重启次数，得到 %+v", rows[0])
	}
	if !strings.Contains(r.Reply, "demo下有 1 个 Pod") {
		t.Fatalf("回复里应有数量摘要，得到 %q", r.Reply)
	}
}

// TestStubListWorkloadPodsQuestion 覆盖桩模型路径的 Deployment 版：
// 用户问“demo-api 下有几个 Pod”，桩模型调 list_workload_pods，
// structured 写 workloadPods 那个 key，行也是完整的。
func TestStubListWorkloadPodsQuestion(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)

	r, err := svc.RunMessage(ctx, conv, "demo-api 下有几个 Pod")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if !toolCalled(r, "list_workload_pods") {
		t.Fatalf("问某 Deployment 下的 Pod 时应调 list_workload_pods，调了 %+v", r.ToolCalls)
	}
	rows := podRowsFromStructured(t, r.Structured["workloadPods"])
	if len(rows) != 1 || rows[0]["name"] != kube.DemoPod {
		t.Fatalf("structured[\"workloadPods\"] 应是 demo-api 下 Pod 的完整行，得到 %+v", rows)
	}
	if r.Structured["pods"] != nil {
		t.Fatalf("本轮没调 list_pods，structured 里不应有 pods，得到 %+v", r.Structured["pods"])
	}
}

// TestRealListPodsCanonicalKey 覆盖真模型路径的规范 key 映射：
// 模型调 list_pods，structured 写 "pods"，行是完整的；
// 本轮没调 list_workload_pods，workloadPods 那个 key 不应出现。
func TestRealListPodsCanonicalKey(t *testing.T) {
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"list_pods","arguments":"{\"namespace\":\"demo\"}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"demo 下只有 1 个 Pod。"}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo 命名空间下有哪些 Pod")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("应发 2 次请求（工具调用 + 最终文本），得到 %d 次", got)
	}
	rows := podRowsFromStructured(t, r.Structured["pods"])
	if len(rows) != 1 || rows[0]["name"] != kube.DemoPod || rows[0]["namespace"] != "demo" {
		t.Fatalf("structured[\"pods\"] 应是 demo 下全部 Pod 的完整行，得到 %+v", rows)
	}
	if r.Structured["workloadPods"] != nil {
		t.Fatalf("本轮没调 list_workload_pods，structured 里不应有 workloadPods，得到 %+v", r.Structured["workloadPods"])
	}
}

// TestRealListWorkloadPodsCanonicalKey 覆盖真模型路径的另一边：
// 模型调 list_workload_pods，structured 写 "workloadPods"（不再是 "pods"），
// 行是完整的；本轮没调 list_pods，pods 那个 key 不应出现。
// 以前这里写的是 "pods"，前端按 problemPods ?? pods 读到名字字符串
// 数组就渲出一排空行。
func TestRealListWorkloadPodsCanonicalKey(t *testing.T) {
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"list_workload_pods","arguments":"{\"namespace\":\"demo\",\"deployment\":\"demo-api\"}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"demo-api 下只有 1 个 Pod。"}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 下有几个 Pod")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("应发 2 次请求（工具调用 + 最终文本），得到 %d 次", got)
	}
	rows := podRowsFromStructured(t, r.Structured["workloadPods"])
	if len(rows) != 1 || rows[0]["name"] != kube.DemoPod || rows[0]["deployment"] != "demo-api" {
		t.Fatalf("structured[\"workloadPods\"] 应是 demo-api 下 Pod 的完整行，得到 %+v", rows)
	}
	if r.Structured["pods"] != nil {
		t.Fatalf("本轮没调 list_pods，structured 里不应有 pods，得到 %+v", r.Structured["pods"])
	}
}
