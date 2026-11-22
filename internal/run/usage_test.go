package run

import (
	"context"
	"testing"

	"kubecause/internal/agent"
	"kubecause/internal/kube"
	"kubecause/internal/store"
)

// usageModel 按轮次返回带用量的确定性剧本：
// 第 0 轮调 get_pod（用量 100/50/150），第 1 轮交报告（用量 10/5/15）。
// 用来验证正式诊断两次调用的用量都累加进了任务。
type usageModel struct{}

func (usageModel) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	switch in.StepIndex {
	case 0:
		return agent.ModelStep{
			ToolCalls: []*agent.ToolCall{{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}},
			Usage:     store.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}, nil
	default:
		return agent.ModelStep{
			IsFinal: true,
			Report: &agent.Report{
				Facts: []string{"Pod 重启 5 次"}, Cause: "容器内存限制过低",
				EvidenceIDs: []string{"ev-1"}, Recommendations: []string{"调高 limit"}, Confidence: "high",
			},
			Usage: store.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}, nil
	}
}

func TestWorkerAccumulatesUsageIntoRun(t *testing.T) {
	// 正式诊断：两轮 Next 的用量都要累加进任务，
	// 包括最后交报告的那一次；恢复后从库里读的累计值保留、只做加法。
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	w := NewWorker(s, &ToolsAdapter{Kube: kube.NewFake()}, usageModel{})
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应跑完任务: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if got.Status != store.StatusCompleted {
		t.Fatalf("任务应完成: %s %s", got.Status, got.Error)
	}
	if want := (store.Usage{PromptTokens: 110, CompletionTokens: 55, TotalTokens: 165}); got.Usage != want {
		t.Fatalf("两轮用量应累加为 %+v，得到 %+v", want, got.Usage)
	}
}
