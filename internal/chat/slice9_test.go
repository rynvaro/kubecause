package chat

import (
	"context"
	"encoding/json"
	"fmt"
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

func newRealService(t *testing.T, srv *httptest.Server) (*Service, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	return NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real), s
}

// TestRealMultiRoundImageChoice 覆盖多轮追问选镜像：
// 第一轮模型问“换成 nginx:1.25 还是 nginx:latest？”，
// 用户回“换成 nginx:1.25”，第二轮模型把用户的回答转成结构化 Patch，
// 建议卡里有 image 的 Patch，而不是再问一遍。
// 多轮记忆靠最近 10 条历史：用户的回答在第二轮的历史里，
// 模型要能用它生成 Patch；后端只负责把历史带给模型，不替模型选值。
func TestRealMultiRoundImageChoice(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1, 2:
			// 第一轮：模型问选哪个镜像（第 2 次是追问结构化建议后的回答，仍问）。
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"镜像 tag 不存在，拉取失败。你想换成哪个镜像？是 nginx:1.25 还是 nginx:latest？"}}]}`)
		default:
			// 第二轮：用户已选 nginx:1.25，模型提交结构化诊断 + Patch。
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-3","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"镜像拉取失败\"],\"cause\":\"镜像 tag 不存在\",\"evidenceIds\":[],\"confidence\":\"high\",\"suggestedRepair\":{\"namespace\":\"demo\",\"deployment\":\"demo-api\",\"container\":\"demo-api\",\"reason\":\"镜像 tag 不存在\",\"patches\":[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/image\",\"value\":\"nginx:1.25\"}]}}"}}]}}]}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	svc, s := newRealService(t, srv)
	conv := newConversation(t, svc)

	r1, err := svc.RunMessage(ctx, conv, "demo 那个一直拉不到镜像的是怎么回事")
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if !strings.Contains(r1.Reply, "nginx:1.25") || !strings.Contains(r1.Reply, "nginx:latest") {
		t.Fatalf("第一轮模型应问选哪个镜像，得到 %q", r1.Reply)
	}
	if r1.SuggestedRepair != nil {
		t.Fatalf("第一轮还没选值，不应有建议卡，得到 %+v", r1.SuggestedRepair)
	}

	r2, err := svc.RunMessage(ctx, conv, "换成 nginx:1.25")
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if r2.SuggestedRepair == nil {
		t.Fatalf("第二轮用户选了镜像后应返回建议卡，得到 %+v", r2)
	}
	sr := r2.SuggestedRepair
	if len(sr.Patches) != 1 {
		t.Fatalf("建议卡应有 1 条 Patch，得到 %+v", sr.Patches)
	}
	if sr.Patches[0].Path != "/spec/template/spec/containers/0/image" || sr.Patches[0].Value != "nginx:1.25" {
		t.Fatalf("建议卡应是换镜像到 nginx:1.25 的 Patch，得到 %+v", sr.Patches)
	}
	// 刷新后建议卡还在：Structured 里有 suggestedRepair，且 patches 完整。
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
		t.Fatalf("Structured 应是合法 JSON: %v", err)
	}
	srJSON, ok := structured["suggestedRepair"].(map[string]any)
	if !ok {
		t.Fatalf("刷新后 Structured 里仍应有 suggestedRepair，得到 %v", structured)
	}
	patches, ok := srJSON["patches"].([]any)
	if !ok || len(patches) != 1 {
		t.Fatalf("刷新后建议卡的 patches 应保留，得到 %v", srJSON)
	}
}

// TestRealProactiveImageChange 覆盖主动变更：
// 用户直接说“把 demo-api 的镜像换成 nginx:1.26”，Pod 是健康的，
// 模型先调 get_deployment 查当前值，再直接给 Patch 建议卡，
// 不用先调 list_problem_pods、get_pod、get_events 这些诊断工具。
func TestRealProactiveImageChange(t *testing.T) {
	var calls int64
	var toolNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			toolNames = append(toolNames, "get_deployment")
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_deployment","arguments":"{\"namespace\":\"demo\",\"deployment\":\"demo-api\"}"}}]}}]}`)
		default:
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"用户要求换镜像\"],\"cause\":\"用户主动要求把镜像换成 nginx:1.26\",\"evidenceIds\":[],\"confidence\":\"high\",\"suggestedRepair\":{\"namespace\":\"demo\",\"deployment\":\"demo-api\",\"container\":\"demo-api\",\"reason\":\"用户主动要求换镜像\",\"patches\":[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/image\",\"value\":\"nginx:1.26\"}]}}"}}]}}]}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)

	reply, err := svc.RunMessage(ctx, conv, "把 demo-api 的镜像换成 nginx:1.26")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if reply.SuggestedRepair == nil {
		t.Fatalf("主动换镜像应返回建议卡，得到 %+v", reply)
	}
	sr := reply.SuggestedRepair
	if len(sr.Patches) != 1 || sr.Patches[0].Path != "/spec/template/spec/containers/0/image" || sr.Patches[0].Value != "nginx:1.26" {
		t.Fatalf("建议卡应是换镜像到 nginx:1.26 的 Patch，得到 %+v", sr.Patches)
	}
	// 模型先查了当前值：本轮工具调用里有 get_deployment。
	found := false
	for _, tc := range reply.ToolCalls {
		if tc.Name == "get_deployment" {
			found = true
		}
		if tc.Name == "list_problem_pods" || tc.Name == "get_pod" || tc.Name == "get_events" {
			t.Fatalf("主动变更不应先调诊断工具，调了 %s", tc.Name)
		}
	}
	if !found {
		t.Fatalf("主动变更应先调 get_deployment 查当前值，工具调用为 %+v", reply.ToolCalls)
	}
	_ = toolNames
}

// TestStubProactiveImageChange 覆盖 Stub 路径的主动变更：
// 健康 Pod 换镜像版本，不用诊断，直接查当前镜像、给 Patch 卡。
func TestStubProactiveImageChange(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	reply, err := svc.RunMessage(ctx, conv, "把 demo-api 的镜像换成 nginx:1.26")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if reply.SuggestedRepair == nil {
		t.Fatalf("主动换镜像应返回建议卡，得到 %+v", reply)
	}
	sr := reply.SuggestedRepair
	if len(sr.Patches) != 1 || sr.Patches[0].Path != "/spec/template/spec/containers/0/image" || sr.Patches[0].Value != "nginx:1.26" {
		t.Fatalf("建议卡应是换镜像到 nginx:1.26 的 Patch，得到 %+v", sr.Patches)
	}
	if sr.OldValue != "demo-api:latest" {
		t.Fatalf("建议卡的旧值应是当前镜像 demo-api:latest，得到 %q", sr.OldValue)
	}
	// 只调了 get_deployment，没做故障诊断。
	for _, tc := range reply.ToolCalls {
		if tc.Name == "list_problem_pods" || tc.Name == "get_pod" || tc.Name == "get_events" {
			t.Fatalf("主动变更不应调诊断工具，调了 %s", tc.Name)
		}
	}
	// 对话只读：Deployment 的镜像没变。
	dep, _ := svc.Tools.GetDeployment(ctx, "demo", "demo-api")
	b, _ := json.Marshal(dep.Data)
	if !strings.Contains(string(b), "demo-api:latest") {
		t.Fatalf("对话后镜像应仍是 demo-api:latest，得到 %s", b)
	}
}

// TestStubImagePullGivesImageCard 覆盖 Stub 路径的 ImagePullBackOff 也给卡：
// 不再只有 OOM 能触发修复建议，镜像拉取失败给一张镜像 Patch 卡，
// 而不是一张内存修复卡。
func TestStubImagePullGivesImageCard(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	tools := &run.ToolsAdapter{Kube: kube.NewFakeWithScenario("imagepull")}
	svc := NewService(s, tools, model.NewStub())
	conv := newConversation(t, svc)
	reply, err := svc.RunMessage(ctx, conv, "demo 那个一直拉不到镜像的是怎么回事")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	if reply.SuggestedRepair == nil {
		t.Fatalf("ImagePullBackOff 也应返回修复建议卡，得到 %+v", reply)
	}
	sr := reply.SuggestedRepair
	if len(sr.Patches) != 1 || sr.Patches[0].Path != "/spec/template/spec/containers/0/image" {
		t.Fatalf("镜像场景的建议卡应是 image Patch，得到 %+v", sr.Patches)
	}
	if sr.Patches[0].Value != "demo-api:latest" {
		t.Fatalf("镜像场景的建议卡新值应为 demo-api:latest，得到 %v", sr.Patches[0].Value)
	}
	// 建议卡是通用 Patch 格式：Structured 里也有 patches。
	b, _ := json.Marshal(reply.Structured)
	if !strings.Contains(string(b), "demo-api:bad-tag") && !strings.Contains(string(b), "demo-api:latest") {
		t.Fatalf("结构化结果里应有镜像信息，得到 %s", b)
	}
}