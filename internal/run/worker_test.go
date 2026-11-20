package run

import (
	"context"
	"testing"

	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/store"
)

func TestWorkerRestartRecoveryNoDuplicate(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	tools := &ToolsAdapter{Kube: fake}
	w := NewWorker(s, tools, model.NewStub())

	// 创建任务并手动写入前两步，模拟进程在第 2 步后崩掉。
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	prior := []store.RunStep{
		{RunID: r.ID, Seq: 1, Kind: "tool_result", ToolName: "list_workload_pods", CallID: "call-1", ArgsJSON: `{"deployment":"demo-api","namespace":"demo"}`, Summary: "Deployment demo-api 下有 1 个 Pod"},
		{RunID: r.ID, Seq: 2, Kind: "tool_result", ToolName: "get_pod", CallID: "call-2", ArgsJSON: `{"namespace":"demo","pod":"demo-api-7d9f6c4b5-xk2v9"}`, Summary: "Pod demo-api-7d9f6c4b5-xk2v9 状态 Running，重启 5 次，上次终止原因 OOMKilled"},
	}
	for _, st := range prior {
		st := st
		if err := s.AppendStep(ctx, &st); err != nil {
			t.Fatalf("写已有步骤失败: %v", err)
		}
	}
	// 恢复后 Worker 重新领取，应从第 3 步继续，前两步的工具不再重复调。
	ran, err := w.RunOne(ctx)
	if err != nil || !ran {
		t.Fatalf("Worker 应领到任务: ran=%v err=%v", ran, err)
	}
	if fake.CallCount["ListPodsByLabels"] != 0 {
		t.Fatalf("list_workload_pods 不应重复执行，调用次数=%d", fake.CallCount["ListPodsByLabels"])
	}
	if fake.CallCount["GetPod"] != 0 {
		t.Fatalf("get_pod 不应重复执行，调用次数=%d", fake.CallCount["GetPod"])
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("恢复后任务应完成，状态=%s err=%s", got.Status, got.Error)
	}
	if got.ReportJSON == "" {
		t.Fatalf("恢复后应有诊断报告")
	}
	steps, _ := s.ListSteps(ctx, r.ID)
	seen := map[int]bool{}
	for _, st := range steps {
		if seen[st.Seq] {
			t.Fatalf("序号 %d 重复出现", st.Seq)
		}
		seen[st.Seq] = true
	}
	// 还应生成一份待审批提案（OOM + Deployment + 高置信度）。
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil || proposal.Status != store.ChangePending {
		t.Fatalf("应生成待审批提案: %v %+v", err, proposal)
	}
	if proposal.OldValue != "128Mi" || proposal.NewValue != "256Mi" {
		t.Fatalf("提案新旧值不对: %+v", proposal)
	}
}

func TestWorkerFullRunFakeStub(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应跑完任务: %v", err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("任务应完成: %s %s", got.Status, got.Error)
	}
	// 报告里必须提到内存 / OOM。
	if !contains(got.ReportJSON, "内存") && !contains(got.ReportJSON, "OOM") {
		t.Fatalf("报告应指向内存/OOM: %s", got.ReportJSON)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
