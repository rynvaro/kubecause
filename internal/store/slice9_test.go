package store

import (
	"context"
	"testing"
)

// TestChangeRequestPatchesPersist 覆盖 Slice 9 的提案 Patch 持久化：
// 通用 Patch 列表存进 change_requests，读出来原样回来；
// 老提案（没有 patches_json）读出来从 old/new 值反推一条内存 Patch。
func TestChangeRequestPatchesPersist(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "换镜像"}
	_ = s.CreateRun(ctx, r)
	c, err := s.CreateChangeRequest(ctx, &ChangeRequest{
		RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
		OldValue: "demo-api:latest", NewValue: "nginx:1.25",
		Patches: []PatchOperation{
			{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "nginx:1.25"},
		},
	})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	got, err := s.GetChangeRequest(ctx, c.ID)
	if err != nil {
		t.Fatalf("读提案失败: %v", err)
	}
	if len(got.Patches) != 1 || got.Patches[0].Path != "/spec/template/spec/containers/0/image" || got.Patches[0].Value != "nginx:1.25" {
		t.Fatalf("提案的 Patches 应原样读回，得到 %+v", got.Patches)
	}
	// 老提案兼容：直接插一行没有 patches_json 的老数据，读出来应反推内存 Patch。
	r2 := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "老提案"}
	_ = s.CreateRun(ctx, r2)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO change_requests(id, run_id, namespace, deployment, container, old_value, new_value, patches_json, deployment_uid, status, approver, execution_status, verification_status, execution_detail, verification_detail, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"chg_legacy", r2.ID, "demo", "demo-api", "demo-api", "128Mi", "256Mi", "", "", ChangePending, "", ExecNotStarted, VerifyNotStarted, "", "", now(), now())
	if err != nil {
		t.Fatalf("插老提案失败: %v", err)
	}
	legacy, err := s.GetChangeRequest(ctx, "chg_legacy")
	if err != nil {
		t.Fatalf("读老提案失败: %v", err)
	}
	if len(legacy.Patches) != 1 || legacy.Patches[0].Path != "/spec/template/spec/containers/0/resources/limits/memory" || legacy.Patches[0].Value != "256Mi" {
		t.Fatalf("老提案应反推一条内存 Patch，得到 %+v", legacy.Patches)
	}
}
