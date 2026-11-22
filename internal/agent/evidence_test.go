package agent

import (
	"context"
	"testing"

	"kubecause/internal/store"
)

// failureFirstModel 按轮次返回剧本：0 轮一次返回两个调用，
// 第一个是非法调用（get_pod 没给 pod 名，校验拒绝），
// 第二个是正常查询；1 轮交报告，引用成功步骤的 ev-1
// 和失败步骤的 call ID（后者应被过滤掉）。
type failureFirstModel struct{}

func (failureFirstModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	if in.StepIndex == 0 {
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": ""}},
			{ID: "call-2", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}},
		}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod OOMKilled"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"ev-1", "call-1"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestEvidenceIDsStableAcrossRecoveryAfterFailure(t *testing.T) {
	// issue #8：成功工具前插入校验拒绝步骤，恢复前后的证据 ID 必须一致；
	// 失败步骤不能成为报告的有效证据引用。
	// 旧口径：成功步骤运行时是 ev-1，恢复后按 Seq 重建成 ev-2；
	// 失败步骤的 call ID 还会被登记为证据。
	ctx := context.Background()
	rec := &stepRecorder{}
	ag := &Agent{Tools: &recordingTools{}, Model: failureFirstModel{}}
	ag.OnStep = rec.onStep
	if _, err := ag.Run(ctx, budgetTestRun(1), nil); err != nil {
		t.Fatalf("第一段不应报错: %v", err)
	}
	if len(rec.steps) != 2 {
		t.Fatalf("第一段应记 2 步（1 拒绝 + 1 成功），得到 %d", len(rec.steps))
	}
	if rec.steps[0].Error == "" {
		t.Fatalf("第一步应是校验拒绝的错误步，得到 %+v", rec.steps[0])
	}
	if rec.steps[0].EvidenceID != "" {
		t.Fatalf("失败步骤不应分配证据 ID，得到 %q", rec.steps[0].EvidenceID)
	}
	if rec.steps[1].EvidenceID != "ev-1" {
		t.Fatalf("校验拒绝后的第一个成功步骤应是 ev-1，得到 %q", rec.steps[1].EvidenceID)
	}

	// 恢复后交报告：引用 ev-1 应保留，引用失败步骤 call-1 应被过滤。
	ag2 := &Agent{Tools: &recordingTools{}, Model: failureFirstModel{}}
	report, err := ag2.Run(ctx, budgetTestRun(8), rec.steps)
	if err != nil {
		t.Fatalf("恢复后应跑完: %v", err)
	}
	if len(report.EvidenceIDs) != 1 || report.EvidenceIDs[0] != "ev-1" {
		t.Fatalf("报告应只引用 ev-1，得到 %v", report.EvidenceIDs)
	}
}

func TestEvidenceIDsNoConflictAfterMultipleRecoveries(t *testing.T) {
	// issue #8：多工具调用、多次恢复后，证据 ID 不冲突、不丢失。
	// 用 roundBudgetModel（三轮各 2 个调用，报告引用 ev-1..ev-6），
	// 中断两次再恢复跑完。
	ctx := context.Background()

	// 第一段：跑 2 轮（ev-1..ev-4）。
	rec1 := &stepRecorder{}
	ag1 := &Agent{Tools: &recordingTools{}, Model: &roundBudgetModel{}}
	ag1.OnStep = rec1.onStep
	if _, err := ag1.Run(ctx, budgetTestRun(2), nil); err != nil {
		t.Fatalf("第一段不应报错: %v", err)
	}
	if len(rec1.steps) != 4 {
		t.Fatalf("第一段应记 4 步，得到 %d", len(rec1.steps))
	}
	// 第二段：恢复后只跑 1 轮（第 2 轮，ev-5、ev-6）。
	// MaxSteps 用 3：已用 2 轮，还剩 1 轮；用 1 的话剩余预算是 0，一轮都跑不了。
	rec2 := &stepRecorder{}
	ag2 := &Agent{Tools: &recordingTools{}, Model: &roundBudgetModel{}}
	ag2.OnStep = rec2.onStep
	if _, err := ag2.Run(ctx, budgetTestRun(3), rec1.steps); err != nil {
		t.Fatalf("第二段不应报错: %v", err)
	}
	if len(rec2.steps) != 2 {
		t.Fatalf("第二段应记 2 步，得到 %d", len(rec2.steps))
	}
	if rec2.steps[0].EvidenceID != "ev-5" || rec2.steps[1].EvidenceID != "ev-6" {
		t.Fatalf("第二段的证据 ID 应是 ev-5、ev-6，得到 %q、%q", rec2.steps[0].EvidenceID, rec2.steps[1].EvidenceID)
	}
	// 第三段：两次恢复后跑完，报告应引用全部 6 个证据。
	all := append(append([]store.RunStep(nil), rec1.steps...), rec2.steps...)
	ag3 := &Agent{Tools: &recordingTools{}, Model: &roundBudgetModel{}}
	report, err := ag3.Run(ctx, budgetTestRun(8), all)
	if err != nil {
		t.Fatalf("第三段不应报错: %v", err)
	}
	want := []string{"ev-1", "ev-2", "ev-3", "ev-4", "ev-5", "ev-6"}
	if len(report.EvidenceIDs) != len(want) {
		t.Fatalf("报告应引用 6 个证据，得到 %v", report.EvidenceIDs)
	}
	for i, id := range want {
		if report.EvidenceIDs[i] != id {
			t.Fatalf("报告证据 ID 第 %d 个应是 %s，得到 %v", i, id, report.EvidenceIDs)
		}
	}
	// 全部成功步骤的 EvidenceID 按执行顺序应是 ev-1..ev-6，没有重复和跳号。
	seen := map[string]bool{}
	for _, st := range all {
		if st.Error != "" {
			continue
		}
		if st.EvidenceID == "" || seen[st.EvidenceID] {
			t.Fatalf("成功步骤的证据 ID 不应为空或重复，得到 %+v", st)
		}
		seen[st.EvidenceID] = true
	}
}

// oldDataEvidenceModel 模拟老数据恢复：
// priorSteps 里已有一条成功的老步骤（没有 EvidenceID），
// StepIndex 2 再调一个新工具，之后交报告，同时引用老步骤的 call ID
// 和新步骤的 ev-1。
type oldDataEvidenceModel struct{}

func (oldDataEvidenceModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	if in.StepIndex == 2 {
		return ModelStep{ToolCalls: []*ToolCall{{ID: "call-new", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod ok"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"call-old", "ev-1"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestOldEvidenceRowsContributeCallIDOnly(t *testing.T) {
	// issue #8：老数据（没有 evidence_id 列）只贡献 CallID，
	// 新分配的 ev 号从 1 起，不和老数据冲突。
	ctx := context.Background()
	prior := []store.RunStep{
		{RunID: "run_old", Seq: 1, Kind: "tool_result", ToolName: "get_pod", CallID: "call-old",
			ArgsJSON: `{"namespace":"demo","pod":"demo-api-7d9f6c4b5-xk2v9"}`,
			ResultJSON: `{"pod":"demo-api-7d9f6c4b5-xk2v9"}`, Summary: "Pod ok", Round: 1},
	}
	rec := &stepRecorder{}
	ag := &Agent{Tools: &recordingTools{}, Model: oldDataEvidenceModel{}}
	ag.OnStep = rec.onStep
	report, err := ag.Run(ctx, budgetTestRun(8), prior)
	if err != nil {
		t.Fatalf("老数据恢复不应报错: %v", err)
	}
	if len(rec.steps) != 1 {
		t.Fatalf("应新执行 1 个调用，得到 %d 步", len(rec.steps))
	}
	if rec.steps[0].EvidenceID != "ev-1" {
		t.Fatalf("老数据之后的新成功步骤应从 ev-1 起编，得到 %q", rec.steps[0].EvidenceID)
	}
	if len(report.EvidenceIDs) != 2 || report.EvidenceIDs[0] != "call-old" || report.EvidenceIDs[1] != "ev-1" {
		t.Fatalf("报告应引用 call-old 和 ev-1，得到 %v", report.EvidenceIDs)
	}
}
