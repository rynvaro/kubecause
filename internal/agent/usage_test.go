package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kubecause/internal/store"
)

// usageTestModel 按轮次返回带用量的确定性剧本：
// 第 0 轮调 get_pod（用量 10/5/15），第 1 轮交报告（用量 20/10/30）。
// 用来验证 Agent 每轮 Next 成功后都把用量交给 OnUsage，
// 包括最后交报告的那一次。
type usageTestModel struct{}

func (usageTestModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	switch in.StepIndex {
	case 0:
		return ModelStep{
			ToolCalls: []*ToolCall{{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}},
			Usage:     store.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}, nil
	default:
		return ModelStep{
			IsFinal: true,
			Report: &Report{
				Facts: []string{"Pod 重启 5 次"}, Cause: "容器内存限制过低",
				EvidenceIDs: []string{"ev-1"}, Recommendations: []string{"调高 limit"}, Confidence: "high",
			},
			Usage: store.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30},
		}, nil
	}
}

func TestAgentReportsUsagePerModelCall(t *testing.T) {
	ctx := context.Background()
	var got []store.Usage
	ag := &Agent{
		Tools: &recordingTools{}, Model: usageTestModel{},
		OnUsage: func(ctx context.Context, u store.Usage) error {
			got = append(got, u)
			return nil
		},
	}
	rep, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err != nil {
		t.Fatalf("诊断失败: %v", err)
	}
	if rep == nil || rep.Cause != "容器内存限制过低" {
		t.Fatalf("报告不对: %+v", rep)
	}
	// 两轮 Next 各调一次 OnUsage：工具轮和交报告轮的用量都要记，
	// 不能因为最后只返回一份报告就把交报告那次的用量丢了。
	if len(got) != 2 {
		t.Fatalf("两轮 Next 应各调一次 OnUsage，得到 %d 次: %+v", len(got), got)
	}
	if want := (store.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); got[0] != want {
		t.Fatalf("第 1 次 OnUsage 应为 %+v，得到 %+v", want, got[0])
	}
	if want := (store.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}); got[1] != want {
		t.Fatalf("第 2 次 OnUsage 应为 %+v，得到 %+v", want, got[1])
	}
}

// zeroUsageModel 直接交报告且不带任何用量（桩模型、或响应里没带 usage）。
type zeroUsageModel struct{}

func (zeroUsageModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	return ModelStep{
		IsFinal: true,
		Report: &Report{
			Facts: []string{"无"}, Cause: "证据不足",
			EvidenceIDs: []string{}, Recommendations: []string{"补查"}, Confidence: "low",
		},
	}, nil
}

func TestAgentSkipsZeroUsage(t *testing.T) {
	ctx := context.Background()
	called := 0
	ag := &Agent{
		Tools: &recordingTools{}, Model: zeroUsageModel{},
		OnUsage: func(ctx context.Context, u store.Usage) error {
			called++
			return nil
		},
	}
	if _, err := ag.Run(ctx, budgetTestRun(8), nil); err != nil {
		t.Fatalf("诊断失败: %v", err)
	}
	if called != 0 {
		t.Fatalf("用量为零时不应调 OnUsage，得到 %d 次", called)
	}
}

// failWithUsageModel 第一次 Next 就返回用量 + 错误，
// 用来验证 Next 出错时 Agent 也记账，且记账失败不掩盖模型错误。
type failWithUsageModel struct{}

func (failWithUsageModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	return ModelStep{Usage: store.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}, fmt.Errorf("模型挂了")
}

func TestAgentRecordsUsageEvenWhenNextFails(t *testing.T) {
	// 审查第 1 项：Next 出错返回里带了用量，Agent 也要在返回模型错误之前
	// 先调 OnUsage 把它记下来，不能因为这轮失败了就丢了已花的 token。
	ctx := context.Background()
	var got []store.Usage
	ag := &Agent{
		Tools: &recordingTools{}, Model: failWithUsageModel{},
		OnUsage: func(ctx context.Context, u store.Usage) error {
			got = append(got, u)
			return nil
		},
	}
	_, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err == nil {
		t.Fatalf("Next 出错时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "模型决策失败") {
		t.Fatalf("应返回模型决策失败，得到 %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Next 出错时也应调一次 OnUsage 记账，得到 %d 次: %+v", len(got), got)
	}
	if want := (store.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}); got[0] != want {
		t.Fatalf("OnUsage 应拿到 %+v，得到 %+v", want, got[0])
	}
}

func TestAgentUsageWriteFailureDoesNotMaskModelError(t *testing.T) {
	// 出错路径上记账只做 best-effort：OnUsage 失败时忽略，
	// 返回的仍是原来的模型错误，不能被写库错误掩盖。
	ctx := context.Background()
	ag := &Agent{
		Tools: &recordingTools{}, Model: failWithUsageModel{},
		OnUsage: func(ctx context.Context, u store.Usage) error {
			return fmt.Errorf("db write failed")
		},
	}
	_, err := ag.Run(ctx, budgetTestRun(8), nil)
	if err == nil {
		t.Fatalf("Next 出错时 Run 应返回错误")
	}
	if !strings.Contains(err.Error(), "模型决策失败") {
		t.Fatalf("应返回原来的模型错误，得到 %v", err)
	}
	if strings.Contains(err.Error(), "db write failed") {
		t.Fatalf("记账失败不应掩盖模型错误，得到 %v", err)
	}
}
