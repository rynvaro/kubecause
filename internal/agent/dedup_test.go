package agent

import (
	"context"
	"fmt"
	"testing"
)

// requeryModel 按轮次返回剧本：
// 0 轮调 get_pod；1 轮用相同参数再调一次 get_pod；2 轮交报告，
// 引用两次调用的 call ID。
type requeryModel struct{}

func (requeryModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	podArgs := map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}
	switch in.StepIndex {
	case 0:
		return ModelStep{ToolCalls: []*ToolCall{{ID: "call-1", Name: "get_pod", Args: podArgs}}}, nil
	case 1:
		return ModelStep{ToolCalls: []*ToolCall{{ID: "call-2", Name: "get_pod", Args: podArgs}}}, nil
	default:
		return ModelStep{IsFinal: true, Report: &Report{
			Facts:           []string{"两次查询 Pod 状态一致"},
			Cause:           "容器内存限制过低",
			EvidenceIDs:     []string{"call-1", "call-2"},
			Recommendations: []string{"调高 limit"},
			OpenQuestions:   []string{},
			Confidence:      "medium",
		}}, nil
	}
}

func TestReQueryAcrossRoundsExecutes(t *testing.T) {
	// issue #7：不同轮次对相同资源的两次合法查询都可以执行
	// （例如修完想再看一眼 Pod 状态），不能被指纹去重静默跳过。
	ctx := context.Background()
	tools := &recordingTools{}
	rec := &stepRecorder{}
	ag := &Agent{Tools: tools, Model: requeryModel{}}
	ag.OnStep = rec.onStep
	report, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err != nil {
		t.Fatalf("重查不应报错: %v", err)
	}
	getPodCalls := 0
	for _, name := range tools.calls {
		if name == "get_pod" {
			getPodCalls++
		}
	}
	if getPodCalls != 2 {
		t.Fatalf("两轮重查应执行 2 次 get_pod，得到 %v", tools.calls)
	}
	if len(rec.steps) != 2 {
		t.Fatalf("应记 2 步，得到 %d", len(rec.steps))
	}
	if rec.steps[0].CallID == rec.steps[1].CallID || rec.steps[0].Round == rec.steps[1].Round {
		t.Fatalf("两次查询应是不同 call ID、不同轮次，得到 %+v", rec.steps)
	}
	for _, id := range []string{"call-1", "call-2"} {
		found := false
		for _, kept := range report.EvidenceIDs {
			if kept == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("报告应引用两次查询的证据 %s，得到 %v", id, report.EvidenceIDs)
		}
	}
}

// duplicateBatchModel 第 0 轮一次返回两个相同的 get_pod 调用
// （不同 call ID），第 1 轮交报告。
type duplicateBatchModel struct{}

func (duplicateBatchModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	if in.StepIndex == 0 {
		args := map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: args},
			{ID: "call-2", Name: "get_pod", Args: args},
		}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod 状态已确认"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"call-1", "call-2"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestSameBatchDuplicateExecutesOnce(t *testing.T) {
	// issue #7：同一批里重复调用只执行一次，第二次拿到第一次的结果，
	// 并且有一步记录（不是静默跳过），模型能按 call ID 对上。
	ctx := context.Background()
	tools := &recordingTools{}
	rec := &stepRecorder{}
	ag := &Agent{Tools: tools, Model: duplicateBatchModel{}}
	ag.OnStep = rec.onStep
	report, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err != nil {
		t.Fatalf("同批重复不应报错: %v", err)
	}
	getPodCalls := 0
	for _, name := range tools.calls {
		if name == "get_pod" {
			getPodCalls++
		}
	}
	if getPodCalls != 1 {
		t.Fatalf("同批重复应只执行 1 次 get_pod，得到 %v", tools.calls)
	}
	if len(rec.steps) != 2 {
		t.Fatalf("同批重复应记 2 步（1 执行 + 1 回放），得到 %d", len(rec.steps))
	}
	if rec.steps[0].ResultJSON == "" || rec.steps[1].ResultJSON != rec.steps[0].ResultJSON {
		t.Fatalf("回放步应和第一次有相同的 ResultJSON，得到 %+v", rec.steps)
	}
	if rec.steps[1].Summary != rec.steps[0].Summary {
		t.Fatalf("回放步应和第一次有相同的 Summary，得到 %+v", rec.steps)
	}
	for _, id := range []string{"call-1", "call-2"} {
		found := false
		for _, kept := range report.EvidenceIDs {
			if kept == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("报告应引用 %s，得到 %v", id, report.EvidenceIDs)
		}
	}
}

// failFirstGetPodTools 的 GetPod 第一次失败，之后成功；
// 其他工具走 recordingTools 的成功实现。
type failFirstGetPodTools struct {
	*recordingTools
	getPodCalls int
}

func (f *failFirstGetPodTools) GetPod(ctx context.Context, ns, name string) (ToolOutput, error) {
	f.getPodCalls++
	if f.getPodCalls == 1 {
		return ToolOutput{}, fmt.Errorf("cluster unreachable")
	}
	return f.recordingTools.GetPod(ctx, ns, name)
}

// failRetryBatchModel 第 0 轮一次返回两个相同的 get_pod 调用，
// 第 1 轮交报告，引用第二次调用的 call ID。
type failRetryBatchModel struct{}

func (failRetryBatchModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	if in.StepIndex == 0 {
		args := map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: args},
			{ID: "call-2", Name: "get_pod", Args: args},
		}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod 查询成功"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"call-2"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestFailedCallRetriesInSameBatch(t *testing.T) {
	// issue #7：失败的调用不进同批去重表，同一批里重复出现就再执行一次。
	ctx := context.Background()
	tools := &failFirstGetPodTools{recordingTools: &recordingTools{}}
	rec := &stepRecorder{}
	ag := &Agent{Tools: tools, Model: failRetryBatchModel{}}
	ag.OnStep = rec.onStep
	report, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err != nil {
		t.Fatalf("失败重试不应报错: %v", err)
	}
	if tools.getPodCalls != 2 {
		t.Fatalf("同一批里失败调用应再执行一次，GetPod 调用次数=%d", tools.getPodCalls)
	}
	if len(rec.steps) != 2 {
		t.Fatalf("应记 2 步（1 失败 + 1 成功），得到 %d", len(rec.steps))
	}
	if rec.steps[0].Error == "" || rec.steps[1].Error != "" {
		t.Fatalf("第一步应是失败步、第二步应是成功步，得到 %+v", rec.steps)
	}
	found := false
	for _, id := range report.EvidenceIDs {
		if id == "call-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("报告应引用重试成功的 call-2，得到 %v", report.EvidenceIDs)
	}
}

// failThenReportModel 按轮次返回剧本：0 轮和 1 轮调 get_pod，2 轮交报告。
// 恢复测试里用它重发失败过的调用；callID 由调用方指定，
// 这样两段运行里的调用实例可以区分开。
// 注意 1 轮也要调 get_pod：第一段只跑了第 0 轮就中断，恢复时从
// 第 1 轮继续（Round 列的 -1 哨兵已把“只跑了第 0 轮”和老数据区分开），
// 重试就发生在第 1 轮。
type failThenReportModel struct{ callID string }

func (m failThenReportModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	if in.StepIndex <= 1 {
		return ModelStep{ToolCalls: []*ToolCall{{ID: m.callID, Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod 查询成功"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{m.callID},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestFailedCallRetriesAfterRecovery(t *testing.T) {
	// issue #7：已记录的失败查询在恢复后可以重新执行——
	// 失败步骤不进指纹，恢复后模型重发相同调用时不再被静默跳过。
	ctx := context.Background()
	// 第一段：get_pod 失败，中断（MaxSteps=1）。
	rec := &stepRecorder{}
	tools1 := &failFirstGetPodTools{recordingTools: &recordingTools{}}
	ag1 := &Agent{Tools: tools1, Model: failThenReportModel{callID: "call-1"}}
	ag1.OnStep = rec.onStep
	if _, err := ag1.Run(ctx, budgetTestRun(1), nil); err != nil {
		t.Fatalf("第一段不应报错: %v", err)
	}
	if len(rec.steps) != 1 || rec.steps[0].Error == "" {
		t.Fatalf("第一段应记 1 个失败步，得到 %+v", rec.steps)
	}
	// 第二段：恢复后模型重发 get_pod（换了 call ID），应执行成功。
	// 临时错误已过，用正常工具。
	tools2 := &recordingTools{}
	ag2 := &Agent{Tools: tools2, Model: failThenReportModel{callID: "call-2"}}
	ag2.OnStep = (&stepRecorder{}).onStep
	report, err := ag2.Run(ctx, budgetTestRun(8), rec.steps)
	if err != nil {
		t.Fatalf("恢复后应跑完: %v", err)
	}
	getPodCalls := 0
	for _, name := range tools2.calls {
		if name == "get_pod" {
			getPodCalls++
		}
	}
	if getPodCalls != 1 {
		t.Fatalf("恢复后重发的 get_pod 应执行 1 次，得到 %v", tools2.calls)
	}
	found := false
	for _, id := range report.EvidenceIDs {
		if id == "call-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("报告应引用重试成功的 call-2，得到 %v", report.EvidenceIDs)
	}
}
