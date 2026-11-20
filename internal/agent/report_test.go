package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReportNormalizeEmptyArrays 覆盖 Slice 7 修的白屏根因：
// 真模型返回的 JSON 缺字段时，Go 解出来是 nil，直接序列化成 null，
// 前端读 report.facts.length 会抛错、整个页面白屏。
// Normalize 后这些字段必须是空数组，序列化出来是 []，前端读 length 得到 0。
func TestReportNormalizeEmptyArrays(t *testing.T) {
	rep := &Report{Cause: "容器内存限制过低", Confidence: "medium"}
	rep.Normalize()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("序列化报告失败: %v", err)
	}
	s := string(b)
	for _, field := range []string{`"facts":[]`, `"evidenceIds":[]`, `"recommendations":[]`, `"openQuestions":[]`} {
		if !strings.Contains(s, field) {
			t.Fatalf("归一化后 %s 应序列化为空数组，得到 %s", field, s)
		}
	}
	if strings.Contains(s, "null") {
		t.Fatalf("归一化后序列化结果里不应出现 null，得到 %s", s)
	}
	// 已有值的字段不能被归一化清掉。
	rep2 := &Report{
		Facts: []string{"Pod 重启 10 次"}, Cause: "OOM",
		EvidenceIDs: []string{"ev-1"}, Recommendations: []string{"调大 limit"},
		OpenQuestions: []string{"日志为空"}, Confidence: "high",
	}
	rep2.Normalize()
	if len(rep2.Facts) != 1 || len(rep2.EvidenceIDs) != 1 || len(rep2.Recommendations) != 1 || len(rep2.OpenQuestions) != 1 {
		t.Fatalf("归一化不应清掉已有值，得到 %+v", rep2)
	}
}

// TestValidateReportNormalizes 覆盖单次诊断路径：
// ValidateReport 是 Agent.Run 交最终报告前的必经关口，
// 模型报告里缺失的切片字段在这里也要被补成空数组。
func TestValidateReportNormalizes(t *testing.T) {
	rep := &Report{Cause: "证据不足，无法确定根因", Confidence: "low"}
	ValidateReport(rep, map[string]bool{})
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), `"facts":null`) {
		t.Fatalf("ValidateReport 后 facts 不应是 null，得到 %s", b)
	}
}
