package store

import (
	"context"
	"testing"
)

// TestRunCarriedSuggestionPersists 覆盖 Run 上带建议的持久化（Slice 10）：
// 对话转正式诊断时带过来的修复建议 JSON 和创建规格 JSON，
// 创建任务后读回来应原样一致，Worker 靠它跳过重新诊断。
func TestRunCarriedSuggestionPersists(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	r := &Run{
		Namespace: "demo", Kind: "Deployment", Name: "demo-bad-image",
		Question:      "demo-bad-image 的镜像拉取失败",
		SuggestedJSON: `{"namespace":"demo","deployment":"demo-bad-image","container":"demo-bad-image","patches":[{"op":"replace","path":"/spec/template/spec/containers/0/image","value":"nginx:1.24"}],"reason":"镜像 tag 不存在"}`,
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.SuggestedJSON != r.SuggestedJSON {
		t.Fatalf("读回的 SuggestedJSON 不对: %+v err=%v", got, err)
	}
	r2 := &Run{
		Namespace: "demo", Kind: "Deployment", Name: "web-demo",
		Question:     "创建 Deployment web-demo",
		CreationJSON: `{"namespace":"demo","name":"web-demo","image":"nginx:1.25","replicas":2}`,
	}
	if err := s.CreateRun(ctx, r2); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	got2, err := s.GetRun(ctx, r2.ID)
	if err != nil || got2.CreationJSON != r2.CreationJSON {
		t.Fatalf("读回的 CreationJSON 不对: %+v err=%v", got2, err)
	}
}

// TestChangeRequestKindAndCreationPersist 覆盖创建提案的持久化（Slice 10）：
// kind="create" 的提案和它的创建规格，创建后读回来应原样一致，
// Patch 提案读回来 kind 是 "patch"、Creation 是 nil。
func TestChangeRequestKindAndCreationPersist(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "web-demo", Question: "创建"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	c, err := s.CreateChangeRequest(ctx, &ChangeRequest{
		RunID: r.ID, Namespace: "demo", Deployment: "web-demo", Container: "web-demo",
		NewValue: "nginx:1.25", Kind: "create",
		Creation: &CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.25", Replicas: 2},
	})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	got, err := s.GetChangeRequest(ctx, c.ID)
	if err != nil {
		t.Fatalf("读提案失败: %v", err)
	}
	if got.Kind != "create" || got.Creation == nil {
		t.Fatalf("读回的提案应为创建类型: %+v", got)
	}
	if got.Creation.Image != "nginx:1.25" || got.Creation.Replicas != 2 {
		t.Fatalf("读回的创建规格不对: %+v", got.Creation)
	}
	byRun, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || byRun.ID != c.ID || byRun.Kind != "create" {
		t.Fatalf("按任务读回的提案不对: %+v err=%v", byRun, err)
	}
	// Patch 提案：kind 缺省时读出来是 "patch"，Creation 是 nil。
	r2 := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "修复"}
	if err := s.CreateRun(ctx, r2); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	p, err := s.CreateChangeRequest(ctx, &ChangeRequest{
		RunID: r2.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
		OldValue: "128Mi", NewValue: "256Mi",
		Patches: []PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256Mi"}},
	})
	if err != nil {
		t.Fatalf("创建 Patch 提案失败: %v", err)
	}
	got2, err := s.GetChangeRequest(ctx, p.ID)
	if err != nil || got2.Kind != "patch" || got2.Creation != nil {
		t.Fatalf("Patch 提案读回不对: %+v err=%v", got2, err)
	}
}
