package agent

import (
	"context"
	"strings"
	"testing"

	"kubecause/internal/store"
)

// roundBudgetModel 按轮次（StepIndex）返回剧本：
// 0 轮：2 个工具调用；1 轮：2 个工具调用；2 轮：2 个工具调用；
// 3 轮：交报告。
// 用来验证预算按轮次算：中断恢复后 StepIndex 应从已用轮次继续，
// 而不是从工具调用行数继续。
type roundBudgetModel struct {
	seenStepIndex []int
}

func (m *roundBudgetModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	m.seenStepIndex = append(m.seenStepIndex, in.StepIndex)
	switch in.StepIndex {
	case 0:
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}},
			{ID: "call-2", Name: "get_events", Args: map[string]string{"namespace": "demo", "name": "demo-api-7d9f6c4b5-xk2v9"}},
		}}, nil
	case 1:
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-3", Name: "get_pod_logs", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9", "container": "demo-api", "previous": "false"}},
			{ID: "call-4", Name: "get_deployment", Args: map[string]string{"namespace": "demo", "deployment": "demo-api"}},
		}}, nil
	case 2:
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-5", Name: "list_problem_pods", Args: map[string]string{"namespace": "demo"}},
			{ID: "call-6", Name: "get_cluster_overview", Args: map[string]string{}},
		}}, nil
	default:
		return ModelStep{IsFinal: true, Report: &Report{
			Facts:           []string{"Pod OOMKilled"},
			Cause:           "容器内存限制过低",
			EvidenceIDs:     []string{"ev-1", "ev-2", "ev-3", "ev-4", "ev-5", "ev-6"},
			Recommendations: []string{"调高 limit"},
			OpenQuestions:   []string{},
			Confidence:      "medium",
		}}, nil
	}
}

// stepRecorder 模拟 Worker 的 OnStep：给每一步编序号并记下来，
// 供“中断后恢复”测试把已提交步骤当 priorSteps 传给下一次 Run。
type stepRecorder struct {
	steps []store.RunStep
}

func (r *stepRecorder) onStep(ctx context.Context, step *store.RunStep) error {
	step.Seq = len(r.steps) + 1
	r.steps = append(r.steps, *step)
	return nil
}

func budgetTestRun(maxSteps int) *store.Run {
	return &store.Run{ID: "run_budget", Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启", MaxSteps: maxSteps, TimeoutSeconds: 60}
}

func TestBudgetCountsRoundsNotRows(t *testing.T) {
	// issue #6：三轮各 2 个调用，中断恢复后预算应按轮次算，
	// 而不是按 tool_result 行数算（旧代码：两轮 4 行，恢复后 StepIndex
	// 从 4 起跳，模型直接交报告，丢掉第三轮的证据）。
	ctx := context.Background()

	// 不中断跑完：4 轮，6 个调用，证据 ev-1..ev-6。
	tools := &recordingTools{}
	m := &roundBudgetModel{}
	ag := &Agent{Tools: tools, Model: m}
	report, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err != nil {
		t.Fatalf("不中断应跑完: %v", err)
	}
	if len(report.EvidenceIDs) != 6 {
		t.Fatalf("不中断应有 6 个证据，得到 %v", report.EvidenceIDs)
	}
	if len(tools.calls) != 6 {
		t.Fatalf("不中断应执行 6 个调用，得到 %v", tools.calls)
	}
	if len(m.seenStepIndex) != 4 || m.seenStepIndex[0] != 0 || m.seenStepIndex[1] != 1 || m.seenStepIndex[2] != 2 || m.seenStepIndex[3] != 3 {
		t.Fatalf("不中断模型应看到轮次 0,1,2,3，得到 %v", m.seenStepIndex)
	}

	// 中断后恢复：先跑 2 轮（MaxSteps=2），再拿已提交步骤恢复跑完。
	rec := &stepRecorder{}
	tools1 := &recordingTools{}
	m1 := &roundBudgetModel{}
	ag1 := &Agent{Tools: tools1, Model: m1}
	ag1.OnStep = rec.onStep
	partial, err := ag1.Run(ctx, budgetTestRun(2), nil)
	if err != nil {
		t.Fatalf("第一段不应报错: %v", err)
	}
	if partial == nil || partial.Confidence != "low" {
		t.Fatalf("只跑 2 轮应返回证据不足的低置信报告: %+v", partial)
	}
	if len(rec.steps) != 4 {
		t.Fatalf("第一段应提交 4 步，得到 %d", len(rec.steps))
	}
	wantRounds := []int{0, 0, 1, 1}
	for i, st := range rec.steps {
		if st.Round != wantRounds[i] {
			t.Fatalf("第一段第 %d 步属于第 %d 轮，得到 Round=%d", i, wantRounds[i], st.Round)
		}
	}

	tools2 := &recordingTools{}
	m2 := &roundBudgetModel{}
	ag2 := &Agent{Tools: tools2, Model: m2}
	rec2 := &stepRecorder{}
	ag2.OnStep = rec2.onStep
	report2, err := ag2.Run(ctx, budgetTestRun(8), rec.steps)
	if err != nil {
		t.Fatalf("恢复后应跑完: %v", err)
	}
	// 恢复后模型应从第 2 轮继续，而不是从第 4 轮（旧代码按行数算，
	// StepIndex 从 4 起跳，模型直接交报告，第三轮的证据全丢）。
	if len(m2.seenStepIndex) != 2 || m2.seenStepIndex[0] != 2 || m2.seenStepIndex[1] != 3 {
		t.Fatalf("恢复后模型应看到轮次 2,3，得到 %v", m2.seenStepIndex)
	}
	if len(report2.EvidenceIDs) != 6 {
		t.Fatalf("恢复后应有 6 个证据，得到 %v", report2.EvidenceIDs)
	}
	if len(tools2.calls) != 2 {
		t.Fatalf("恢复后应执行第三轮的 2 个调用，得到 %v", tools2.calls)
	}
	// 第二段提交的步骤属于第 2 轮。
	for _, st := range rec2.steps {
		if st.Round != 2 {
			t.Fatalf("第二段的步骤都属于第 2 轮，得到 Round=%d", st.Round)
		}
	}
}

// TestRecoveryFromRoundZeroIsExact：只跑了第 0 轮就中断，恢复时应从
// 第 1 轮继续，不再重跑第 0 轮。Round 列的 -1 哨兵把“老数据没有 Round 列”
// 和“新数据只跑了第 0 轮”区分开，第 0 轮恢复不再多给一轮预算。
func TestRecoveryFromRoundZeroIsExact(t *testing.T) {
	ctx := context.Background()
	rec := &stepRecorder{}
	ag1 := &Agent{Tools: &recordingTools{}, Model: &roundBudgetModel{}}
	ag1.OnStep = rec.onStep
	if _, err := ag1.Run(ctx, budgetTestRun(1), nil); err != nil {
		t.Fatalf("第一段不应报错: %v", err)
	}
	if len(rec.steps) != 2 {
		t.Fatalf("第一段应提交 2 步，得到 %d", len(rec.steps))
	}
	for _, st := range rec.steps {
		if st.Round != 0 {
			t.Fatalf("第一段的步骤都属于第 0 轮，得到 Round=%d", st.Round)
		}
	}
	m2 := &roundBudgetModel{}
	ag2 := &Agent{Tools: &recordingTools{}, Model: m2}
	if _, err := ag2.Run(ctx, budgetTestRun(8), rec.steps); err != nil {
		t.Fatalf("恢复后应跑完: %v", err)
	}
	if len(m2.seenStepIndex) == 0 || m2.seenStepIndex[0] != 1 {
		t.Fatalf("只有第 0 轮步骤时恢复应从轮次 1 继续，得到 %v", m2.seenStepIndex)
	}
}

// TestRecoveryWithLegacyStepsIsLenient：老数据步骤（Round=-1，未知轮次）
// 恢复时仍按 0 算、宽容地不扣预算。宽容只留给真正分不清的老数据，
// 不再扩大到只跑了第 0 轮的新数据。
func TestRecoveryWithLegacyStepsIsLenient(t *testing.T) {
	ctx := context.Background()
	prior := []store.RunStep{
		{RunID: "run_budget", Seq: 1, Kind: "tool_result", ToolName: "get_pod", CallID: "call-1", ArgsJSON: `{"namespace":"demo","pod":"demo-api-7d9f6c4b5-xk2v9"}`, Summary: "Pod ok", Round: -1},
		{RunID: "run_budget", Seq: 2, Kind: "tool_result", ToolName: "get_events", CallID: "call-2", ArgsJSON: `{"namespace":"demo","name":"demo-api-7d9f6c4b5-xk2v9"}`, Summary: "events", Round: -1},
	}
	m := &roundBudgetModel{}
	ag := &Agent{Tools: &recordingTools{}, Model: m}
	if _, err := ag.Run(ctx, budgetTestRun(8), prior); err != nil {
		t.Fatalf("恢复后应跑完: %v", err)
	}
	if len(m.seenStepIndex) == 0 || m.seenStepIndex[0] != 0 {
		t.Fatalf("全是老数据步骤时恢复应从轮次 0 开始，得到 %v", m.seenStepIndex)
	}
}

// overLimitModel 第 0 轮一次返回 7 个不同的工具调用，第 1 轮交报告。
type overLimitModel struct {
	seenHistory [][]store.RunStep
}

func (m *overLimitModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	m.seenHistory = append(m.seenHistory, append([]store.RunStep(nil), in.History...))
	if in.StepIndex == 0 {
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}},
			{ID: "call-2", Name: "get_events", Args: map[string]string{"namespace": "demo", "name": "demo-api-7d9f6c4b5-xk2v9"}},
			{ID: "call-3", Name: "get_pod_logs", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9", "container": "demo-api", "previous": "false"}},
			{ID: "call-4", Name: "get_deployment", Args: map[string]string{"namespace": "demo", "deployment": "demo-api"}},
			{ID: "call-5", Name: "search_runbooks", Args: map[string]string{"query": "OOMKilled"}},
			{ID: "call-6", Name: "list_namespaces", Args: map[string]string{}},
			{ID: "call-7", Name: "list_deployments", Args: map[string]string{"namespace": "demo"}},
		}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod OOMKilled"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"ev-1", "ev-2", "ev-3", "ev-4", "ev-5"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

func TestPerRoundToolCallCap(t *testing.T) {
	// issue #6：一轮里 7 个调用，默认上限 5 个，
	// 前 5 个执行、后 2 个记错误步告诉模型分轮调，模型下一轮能看到。
	ctx := context.Background()
	tools := &recordingTools{}
	m := &overLimitModel{}
	rec := &stepRecorder{}
	ag := &Agent{Tools: tools, Model: m}
	ag.OnStep = rec.onStep
	run := budgetTestRun(3)
	run.MaxToolCallsPerRound = 0 // 0 表示用默认值 5
	report, err := ag.Run(ctx, run, nil)
	if err != nil {
		t.Fatalf("超限不应让任务报错: %v", err)
	}
	if len(tools.calls) != 5 {
		t.Fatalf("一轮应只执行 5 个调用，得到 %v", tools.calls)
	}
	if len(rec.steps) != 7 {
		t.Fatalf("一轮应记 7 步（5 成功 + 2 超限），得到 %d", len(rec.steps))
	}
	overLimit := 0
	for _, st := range rec.steps {
		if strings.Contains(st.Error, "已达上限") {
			overLimit++
			if st.Round != 0 {
				t.Fatalf("超限步骤属于第 0 轮，得到 Round=%d", st.Round)
			}
		}
	}
	if overLimit != 2 {
		t.Fatalf("应有 2 个超限错误步，得到 %d", overLimit)
	}
	// 第二轮模型应看到超限错误，而不是以为 7 个调用都执行了。
	if len(m.seenHistory) != 2 {
		t.Fatalf("模型应被调 2 轮，得到 %d", len(m.seenHistory))
	}
	seen := false
	for _, st := range m.seenHistory[1] {
		if strings.Contains(st.Error, "已达上限") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("第二轮模型应看到超限错误步")
	}
	if len(report.EvidenceIDs) != 5 {
		t.Fatalf("报告应引用 5 个已执行调用的证据，得到 %v", report.EvidenceIDs)
	}
}
