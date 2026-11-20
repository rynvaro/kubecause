package agent

import (
	"testing"
)

// TestReportNormalizeDedupes 覆盖 Slice 10 的事实去重：
// 报告里的事实、建议、待确认、证据 ID 各出现两次时，
// 归一化后每条只保留一次，顺序不变。
func TestReportNormalizeDedupes(t *testing.T) {
	rep := &Report{
		Facts:           []string{"Deployment demo-api 内存 limit 128Mi", "Pod 重启 10 次", "Deployment demo-api 内存 limit 128Mi"},
		Cause:           "内存限制过低",
		EvidenceIDs:     []string{"ev-1", "ev-2", "ev-1"},
		Recommendations: []string{"调高 limit", "调高 limit"},
		OpenQuestions:   []string{"有没有泄漏", "有没有泄漏"},
		Confidence:      "medium",
	}
	rep.Normalize()
	if len(rep.Facts) != 2 || rep.Facts[0] != "Deployment demo-api 内存 limit 128Mi" || rep.Facts[1] != "Pod 重启 10 次" {
		t.Fatalf("事实去重不对: %v", rep.Facts)
	}
	if len(rep.EvidenceIDs) != 2 {
		t.Fatalf("证据去重不对: %v", rep.EvidenceIDs)
	}
	if len(rep.Recommendations) != 1 || len(rep.OpenQuestions) != 1 {
		t.Fatalf("建议/待确认去重不对: %v / %v", rep.Recommendations, rep.OpenQuestions)
	}
}

// TestValidateReportDropsIncompleteCreation 覆盖创建建议的形状检查：
// 缺名字或镜像的创建建议在校验时被清掉，不会生成创建提案；
// 给全了的保留。
func TestValidateReportDropsIncompleteCreation(t *testing.T) {
	rep := &Report{
		Facts: []string{"用户要求创建"}, Cause: "创建",
		EvidenceIDs: []string{}, Recommendations: []string{}, OpenQuestions: []string{},
		Confidence:        "medium",
		SuggestedCreation: &SuggestedCreation{Namespace: "demo", Name: "", Image: "nginx:1.25"},
	}
	ValidateReport(rep, map[string]bool{})
	if rep.SuggestedCreation != nil {
		t.Fatalf("缺名字的创建建议应被清掉")
	}
	rep.SuggestedCreation = &SuggestedCreation{Namespace: "demo", Name: "web", Image: "nginx:1.25"}
	ValidateReport(rep, map[string]bool{})
	if rep.SuggestedCreation == nil {
		t.Fatalf("给全的创建建议应保留")
	}
}
