package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubecause/internal/agent"
	"kubecause/internal/store"
)

// TestChatConvertWithTargetWinsOverHistory 覆盖 Slice 10 的转换目标 bug：
// 对话两轮后历史里只有 demo-api，但前端传来 demo-bad-image 的目标和 Patch，
// 转出来的任务必须是 demo/demo-bad-image，且带上建议 Patch，
// 不能再猜成历史里最早的 demo-api。
func TestChatConvertWithTargetWinsOverHistory(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	if _, err := svc.RunMessage(ctx, conv, "当前状态怎么样"); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if _, err := svc.RunMessage(ctx, conv, "demo 那个一直重启的是怎么回事"); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	patches := []store.PatchOperation{{
		Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "nginx:1.24",
	}}
	runID, err := svc.ConvertToRun(ctx, conv, ConvertInput{
		Namespace: "demo", Kind: "Deployment", Name: "demo-bad-image",
		Container: "demo-bad-image", Patches: patches,
	})
	if err != nil {
		t.Fatalf("带目标转换失败: %v", err)
	}
	r, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("转换后应能读到任务: %v", err)
	}
	if r.Namespace != "demo" || r.Kind != "Deployment" || r.Name != "demo-bad-image" {
		t.Fatalf("转换目标不对，应为 demo/Deployment/demo-bad-image，得到 %+v", r)
	}
	if !strings.Contains(r.SuggestedJSON, "demo-bad-image") || !strings.Contains(r.SuggestedJSON, "nginx:1.24") {
		t.Fatalf("任务应带上建议 Patch，得到 %s", r.SuggestedJSON)
	}
	var sr agent.SuggestedRepair
	if err := json.Unmarshal([]byte(r.SuggestedJSON), &sr); err != nil {
		t.Fatalf("建议 JSON 解析失败: %v", err)
	}
	if len(sr.ToPatches()) != 1 || sr.ToPatches()[0].Path != "/spec/template/spec/containers/0/image" {
		t.Fatalf("建议 Patch 不对: %+v", sr)
	}
}

// TestChatCreateTwoTurns 覆盖 Stub 的创建多轮：
// 第一轮只给了镜像没给名字，模型先问名字；
// 第二轮只回名字，模型接上文、直接给一张创建建议卡，
// 不用用户把整句话重说一遍。
func TestChatCreateTwoTurns(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	r1, err := svc.RunMessage(ctx, conv, "在 demo 空间下创建一个 Deployment，镜像 nginx:1.25")
	if err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if r1.SuggestedCreation != nil {
		t.Fatalf("第一轮缺名字，不应直接给创建卡: %+v", r1)
	}
	if !strings.Contains(r1.Reply, "名字") {
		t.Fatalf("第一轮应问名字，得到: %s", r1.Reply)
	}
	r2, err := svc.RunMessage(ctx, conv, "叫 web-demo")
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if r2.SuggestedCreation == nil {
		t.Fatalf("第二轮应给创建建议卡，得到: %+v", r2)
	}
	sc := r2.SuggestedCreation
	if sc.Namespace != "demo" || sc.Name != "web-demo" || sc.Image != "nginx:1.25" {
		t.Fatalf("创建卡不对: %+v", sc)
	}
	if sc.Replicas != 1 {
		t.Fatalf("副本数默认应为 1，得到 %d", sc.Replicas)
	}
	if r2.Structured["suggestedCreation"] == nil {
		t.Fatalf("创建卡应写进 Structured，刷新后才不会丢")
	}
}

// TestChatCreateBarePodSuggestsDeployment 覆盖“创建一个 Pod”的处理：
// 不直接创建裸 Pod，按 Deployment 给卡，并在回复里说明原因。
func TestChatCreateBarePodSuggestsDeployment(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "在 demo 空间下创建一个 Pod，镜像 nginx:1.25，叫 web-pod")
	if err != nil {
		t.Fatalf("创建 Pod 失败: %v", err)
	}
	if r.SuggestedCreation == nil {
		t.Fatalf("应给一张 Deployment 创建卡，得到: %+v", r)
	}
	if r.SuggestedCreation.Name != "web-pod" || r.SuggestedCreation.Image != "nginx:1.25" {
		t.Fatalf("创建卡不对: %+v", r.SuggestedCreation)
	}
	if !strings.Contains(r.Reply, "裸 Pod") || !strings.Contains(r.Reply, "Deployment") {
		t.Fatalf("回复应说明裸 Pod 不直接创建、按 Deployment 给卡，得到: %s", r.Reply)
	}
}

// TestChatConvertWithCreation 覆盖带创建规格的转换：
// 前端把创建卡的规格一起传过来，转出来的任务带 CreationJSON，
// Worker 靠它直接生成创建提案；系统命名空间的创建转换直接报错。
func TestChatConvertWithCreation(t *testing.T) {
	ctx := context.Background()
	svc, s := newTestService(t)
	conv := newConversation(t, svc)
	runID, err := svc.ConvertToRun(ctx, conv, ConvertInput{
		Creation: &store.CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.25", Replicas: 2},
	})
	if err != nil {
		t.Fatalf("带创建规格转换失败: %v", err)
	}
	r, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("转换后应能读到任务: %v", err)
	}
	if r.Namespace != "demo" || r.Kind != "Deployment" || r.Name != "web-demo" {
		t.Fatalf("转换目标不对: %+v", r)
	}
	if !strings.Contains(r.CreationJSON, "web-demo") || !strings.Contains(r.CreationJSON, "nginx:1.25") {
		t.Fatalf("任务应带上创建规格，得到 %s", r.CreationJSON)
	}
	if _, err := svc.ConvertToRun(ctx, conv, ConvertInput{
		Creation: &store.CreationSpec{Namespace: "kube-system", Name: "web", Image: "nginx:1.25"},
	}); err == nil {
		t.Fatalf("系统命名空间的创建转换应报错")
	}
}

// TestChatCreateDuplicateName 覆盖创建时同名已存在：
// demo-api 已经在 fake 里，不给创建卡，直接告诉用户已存在。
func TestChatCreateDuplicateName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "在 demo 空间下创建一个 Deployment，镜像 nginx:1.25，叫 demo-api")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if r.SuggestedCreation != nil {
		t.Fatalf("同名已存在时不应给创建卡: %+v", r)
	}
	if !strings.Contains(r.Reply, "已经有一个同名") {
		t.Fatalf("回复应说明同名已存在，得到: %s", r.Reply)
	}
}
