package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kubecause/internal/store"
)

// illegalCallModel 每轮都返回一个非法工具调用，走到校验失败分支。
type illegalCallModel struct{}

func (illegalCallModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	return ModelStep{ToolCalls: []*ToolCall{{ID: "call-1", Name: "delete_pod", Args: map[string]string{"namespace": "demo", "pod": "x"}}}}, nil
}

// failGetPodTools 的 GetPod 恒失败，其他工具走 stubTools 的成功实现。
type failGetPodTools struct{ *stubTools }

func (f failGetPodTools) GetPod(ctx context.Context, ns, name string) (ToolOutput, error) {
	return ToolOutput{}, fmt.Errorf("cluster unreachable")
}

// failOnceModel 第一轮调 get_pod，第二轮交报告，
// 用来验证工具失败本身不终结任务、模型能换路继续。
type failOnceModel struct{ rounds int }

func (m *failOnceModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	m.rounds++
	if m.rounds == 1 {
		return ModelStep{ToolCalls: []*ToolCall{{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}}}, nil
	}
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod 查询失败"},
		Cause:           "集群暂时不可达，无法确认原因",
		EvidenceIDs:     []string{},
		Recommendations: []string{"稍后重试"},
		OpenQuestions:   []string{},
		Confidence:      "low",
	}}, nil
}

func onStepTestRun() *store.Run {
	return &store.Run{ID: "run_onstep", Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启", MaxSteps: 3, TimeoutSeconds: 60}
}

func TestOnStepErrorInValidationBranchStopsRun(t *testing.T) {
	// issue #9：校验失败分支写库失败时，Run 应返回错误停下，
	// 而不是在数据库缺失这一步的情况下继续推进。
	ag := &Agent{Tools: &stubTools{}, Model: illegalCallModel{}}
	ag.OnStep = func(ctx context.Context, step *store.RunStep) error {
		return errors.New("db write failed")
	}
	_, err := ag.Run(context.Background(), onStepTestRun(), nil)
	if err == nil {
		t.Fatalf("OnStep 写库失败时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "db write failed") {
		t.Fatalf("应原样返回写库错误，得到 %v", err)
	}
}

func TestOnStepErrorInExecutionFailureBranchStopsRun(t *testing.T) {
	// issue #9：工具执行失败分支写库失败时，同样应返回错误停下。
	ag := &Agent{Tools: &failGetPodTools{stubTools: &stubTools{}}, Model: &failOnceModel{}}
	ag.OnStep = func(ctx context.Context, step *store.RunStep) error {
		return errors.New("db write failed")
	}
	_, err := ag.Run(context.Background(), onStepTestRun(), nil)
	if err == nil {
		t.Fatalf("OnStep 写库失败时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "db write failed") {
		t.Fatalf("应原样返回写库错误，得到 %v", err)
	}
}

func TestToolFailureStillRetriesWhenOnStepSucceeds(t *testing.T) {
	// 对照：OnStep 正常时，工具失败不终结任务，模型第二轮能交报告。
	m := &failOnceModel{}
	ag := &Agent{Tools: &failGetPodTools{stubTools: &stubTools{}}, Model: m}
	var saved int
	ag.OnStep = func(ctx context.Context, step *store.RunStep) error {
		saved++
		return nil
	}
	report, err := ag.Run(context.Background(), onStepTestRun(), nil)
	if err != nil {
		t.Fatalf("OnStep 正常时工具失败不应让 Run 报错: %v", err)
	}
	if report == nil || m.rounds != 2 {
		t.Fatalf("模型应第二轮交报告，实际 %d 轮，报告 %+v", m.rounds, report)
	}
	if saved != 1 {
		t.Fatalf("失败的工具步应写库一次，实际 %d 次", saved)
	}
}
