package agent

import "testing"

// TestSuggestedRepairToPatchesLegacy 覆盖向后兼容：
// 旧的 memory-only 建议卡（field=oldValue->newValue）
// 要能转成通用 Patch，不让 Slice 1-8 的测试和 eval 崩掉。
func TestSuggestedRepairToPatchesLegacy(t *testing.T) {
	sr := &SuggestedRepair{
		Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
		Field: "memoryLimit", OldValue: "128Mi", NewValue: "256Mi",
		Reason: "OOM",
	}
	patches := sr.ToPatches()
	if len(patches) != 1 {
		t.Fatalf("旧卡应转成 1 条 Patch，得到 %+v", patches)
	}
	if patches[0].Op != "replace" || patches[0].Path != "/spec/template/spec/containers/0/resources/limits/memory" || patches[0].Value != "256Mi" {
		t.Fatalf("旧卡转成的 Patch 不对: %+v", patches)
	}
	// Normalize 之后 Patches 字段应被填上，旧字段留着做展示。
	sr.Normalize()
	if len(sr.Patches) != 1 {
		t.Fatalf("Normalize 后应有 Patches，得到 %+v", sr)
	}
	if sr.OldValue != "128Mi" || sr.NewValue != "256Mi" {
		t.Fatalf("Normalize 不应丢掉旧字段: %+v", sr)
	}
	// 镜像旧字段也能转。
	img := &SuggestedRepair{Field: "image", OldValue: "demo-api:bad-tag", NewValue: "demo-api:latest"}
	img.Normalize()
	if len(img.Patches) != 1 || img.Patches[0].Path != "/spec/template/spec/containers/0/image" {
		t.Fatalf("镜像旧卡应转成 image Patch，得到 %+v", img.Patches)
	}
}

// TestSuggestedRepairPatchesPreferred 覆盖“两个都给了时以 Patches 为准”。
func TestSuggestedRepairPatchesPreferred(t *testing.T) {
	sr := &SuggestedRepair{
		Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
		Field: "memoryLimit", OldValue: "128Mi", NewValue: "256Mi",
		Patches: []PatchOperation{
			{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "nginx:1.25"},
		},
	}
	patches := sr.ToPatches()
	if len(patches) != 1 || patches[0].Path != "/spec/template/spec/containers/0/image" {
		t.Fatalf("有 Patches 时应以 Patches 为准，得到 %+v", patches)
	}
}

// TestValidateReportKeepsGenericPatches 覆盖 ValidateReport 不再只认内存：
// 带 image Patch 的建议不再被清掉；字段映射不到的旧字段才清掉。
func TestValidateReportKeepsGenericPatches(t *testing.T) {
	rep := &Report{
		Facts: []string{"镜像拉取失败"}, Cause: "镜像 tag 不存在",
		EvidenceIDs: []string{"ev-1"}, Confidence: "high",
		SuggestedRepair: &SuggestedRepair{
			Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
			Patches: []PatchOperation{
				{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "nginx:1.25"},
			},
			Reason: "镜像 tag 不存在",
		},
	}
	ValidateReport(rep, map[string]bool{"ev-1": true})
	if rep.SuggestedRepair == nil {
		t.Fatalf("带 image Patch 的建议不应被清掉")
	}
	if len(rep.SuggestedRepair.Patches) != 1 {
		t.Fatalf("建议的 Patches 应保留，得到 %+v", rep.SuggestedRepair)
	}
	// 旧字段 field 写了一个不存在的字段、又没有 Patches 时清掉。
	bad := &Report{
		Facts: []string{"x"}, Cause: "y",
		EvidenceIDs: []string{"ev-1"}, Confidence: "high",
		SuggestedRepair: &SuggestedRepair{
			Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
			Field: "notAField", OldValue: "a", NewValue: "b",
		},
	}
	ValidateReport(bad, map[string]bool{"ev-1": true})
	if bad.SuggestedRepair != nil {
		t.Fatalf("映射不到的旧字段建议应被清掉，得到 %+v", bad.SuggestedRepair)
	}
}
