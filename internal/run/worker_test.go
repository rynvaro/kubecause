package run

import (
	"context"
	"testing"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/repair"
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
		{RunID: r.ID, Seq: 1, Kind: "tool_result", ToolName: "list_workload_pods", CallID: "call-1", ArgsJSON: `{"deployment":"demo-api","namespace":"demo"}`, Summary: "Deployment demo-api 下有 1 个 Pod", Round: 0},
		{RunID: r.ID, Seq: 2, Kind: "tool_result", ToolName: "get_pod", CallID: "call-2", ArgsJSON: `{"namespace":"demo","pod":"demo-api-7d9f6c4b5-xk2v9"}`, Summary: "Pod demo-api-7d9f6c4b5-xk2v9 状态 Running，重启 5 次，上次终止原因 OOMKilled", Round: 1},
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

func TestWorkerRepairFullFlowFakeStub(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	// 修复执行器用写客户端（fake 上读写是同一个对象，但接口分开）。
	w.SetRepair(repair.NewExecutor(s, fake))
	// 把验证超时调小，测试不用等 120 秒。
	w.Repair.VerifyTimeout = 5 * time.Second
	w.Repair.VerifyInterval = 20 * time.Millisecond
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应跑完诊断: %v", err)
	}
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil {
		t.Fatalf("诊断后应生成提案: %v", err)
	}
	// 批准后 Worker 应领取并执行、验证。
	if _, _, err := s.DecideApproval(ctx, proposal.ID, store.ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if ran, err := w.ProcessOneRepair(ctx); err != nil || !ran {
		t.Fatalf("Worker 应执行已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, proposal.ID)
	if got.ExecutionStatus != store.ExecExecuted {
		t.Fatalf("执行状态应为 executed，得到 %s，详情 %s", got.ExecutionStatus, got.ExecutionDetail)
	}
	if got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("验证状态应为 passed，得到 %s，详情 %s", got.VerificationStatus, got.VerificationDetail)
	}
	dep, _ := fake.GetDeployment(ctx, "demo", "demo-api")
	if dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String() != "256Mi" {
		t.Fatalf("执行后 limit 应为 256Mi")
	}
	// 第二次再跑修复应无事可做（提案已执行完，不会重复 Patch）。
	if ran, _ := w.ProcessOneRepair(ctx); ran {
		t.Fatalf("已执行的提案不应被重复领取")
	}
}

func TestWorkerRepairRejectedNoWrite(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	w.SetRepair(repair.NewExecutor(s, fake))
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	if _, err := w.RunOne(ctx); err != nil {
		t.Fatalf("诊断失败: %v", err)
	}
	proposal, _ := s.GetChangeRequestByRun(ctx, r.ID)
	if _, _, err := s.DecideApproval(ctx, proposal.ID, store.ChangeRejected, "varo"); err != nil {
		t.Fatalf("拒绝失败: %v", err)
	}
	if ran, _ := w.ProcessOneRepair(ctx); ran {
		t.Fatalf("拒绝的提案不应被执行")
	}
	dep, _ := fake.GetDeployment(ctx, "demo", "demo-api")
	if dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String() != "128Mi" {
		t.Fatalf("拒绝后 limit 应仍为 128Mi")
	}
}

// TestWorkerNoProposalWhenNewValueMissing 覆盖 Slice 6 的“模型没给新值就不生成提案”：
// 模型报告里给了修复建议但 NewValue 为空时，Worker 不再按旧值翻倍兜底，
// 不生成任何提案。修复决策只能由模型做，后端只做校验。
func TestWorkerNoProposalWhenNewValueMissing(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启", MaxSteps: 8, TimeoutSeconds: 60}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	report := &agent.Report{
		Confidence: "high",
		SuggestedRepair: &agent.SuggestedRepair{
			Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
			Field: "memoryLimit", OldValue: "128Mi", NewValue: "",
			Reason: "容器内存限制过低",
		},
	}
	if err := w.maybeCreateProposal(ctx, r, report); err != nil {
		t.Fatalf("maybeCreateProposal 不应报错: %v", err)
	}
	if p, err := s.GetChangeRequestByRun(ctx, r.ID); err == nil && p != nil {
		t.Fatalf("模型没给新值时不应生成提案，得到 %+v", p)
	}
}
