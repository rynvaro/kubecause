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

// TestWorkerImageProposalFullFlow 覆盖 ImagePullBackOff 走通用 Patch 全流程：
// 模型报告里给一张镜像 Patch，Worker 生成提案、批准、执行、验证，
// 镜像真的换成 demo-api:latest，内存 Patch 的老路不受影响。
func TestWorkerImageProposalFullFlow(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFakeWithScenario("imagepull")
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	w.SetRepair(repair.NewExecutor(s, fake))
	w.Repair.VerifyTimeout = 5 * time.Second
	w.Repair.VerifyInterval = 20 * time.Millisecond
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么拉不到镜像", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	// 直接调 maybeCreateProposal：报告里是镜像 Patch，不是内存。
	report := &agent.Report{
		Confidence: "high",
		SuggestedRepair: &agent.SuggestedRepair{
			Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
			Patches: []agent.PatchOperation{
				{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "demo-api:latest"},
			},
			Reason: "镜像 tag 不存在",
		},
	}
	if err := w.maybeCreateProposal(ctx, r, report); err != nil {
		t.Fatalf("maybeCreateProposal 不应报错: %v", err)
	}
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil {
		t.Fatalf("镜像建议应生成提案: %v %+v", err, proposal)
	}
	if len(proposal.Patches) != 1 || proposal.Patches[0].Value != "demo-api:latest" {
		t.Fatalf("提案应是镜像 Patch，得到 %+v", proposal.Patches)
	}
	if proposal.OldValue != "demo-api:bad-tag" || proposal.NewValue != "demo-api:latest" {
		t.Fatalf("提案展示用的新旧值应是镜像前后值，得到 %q -> %q", proposal.OldValue, proposal.NewValue)
	}
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
	if dep.Spec.Template.Spec.Containers[0].Image != "demo-api:latest" {
		t.Fatalf("执行后镜像应为 demo-api:latest，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

// TestWorkerStubImagePullGeneratesProposal 覆盖 Stub 的镜像建议也能进 Worker：
// imagepull 场景跑完整诊断，报告里应有镜像建议卡，Worker 生成镜像提案。
func TestWorkerStubImagePullGeneratesProposal(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFakeWithScenario("imagepull")
	w := NewWorker(s, &ToolsAdapter{Kube: fake}, model.NewStub())
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么拉不到镜像", MaxSteps: 8, TimeoutSeconds: 60}
	_ = s.CreateRun(ctx, r)
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应跑完诊断: %v", err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("任务应完成: %s %s", got.Status, got.Error)
	}
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil {
		t.Fatalf("镜像场景诊断后应生成提案: %v", err)
	}
	if len(proposal.Patches) != 1 || proposal.Patches[0].Path != "/spec/template/spec/containers/0/image" {
		t.Fatalf("镜像场景的提案应是 image Patch，得到 %+v", proposal.Patches)
	}
}
