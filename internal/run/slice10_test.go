package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// TestWorkerCarriedPatchGeneratesProposal 覆盖 Slice 10 的核心修复：
// 对话转过来的任务带了建议 Patch，Worker 不重新诊断，
// 直接拿 Patch 校验、生成提案。以前是重跑 Agent 循环，
// 8 步用完、置信度 low、没有提案。
// 断言三点：任务完成且置信度不是 low；提案是建议里的镜像 Patch；
// 步骤只有报告一步，没有 8 步工具调用。
func TestWorkerCarriedPatchGeneratesProposal(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFakeWithScenario("imagepull")
	tools := &ToolsAdapter{Kube: fake}
	w := NewWorker(s, tools, model.NewStub())
	w.SetRepair(repair.NewExecutor(s, fake))

	sr := &agent.SuggestedRepair{
		Namespace: "demo", Deployment: "demo-api", Container: "demo-api",
		Field: "image", OldValue: "demo-api:bad-tag", NewValue: "demo-api:latest",
		Reason: "镜像 tag 不存在，拉取失败导致 Pod 卡在 ImagePullBackOff",
	}
	sr.Normalize()
	b, _ := json.Marshal(sr)
	r := &store.Run{
		Namespace: "demo", Kind: "Deployment", Name: "demo-api",
		Question: "demo-api 的镜像拉取失败", MaxSteps: 8, TimeoutSeconds: 60,
		SuggestedJSON: string(b),
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应领到任务: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("带建议的任务应完成，状态=%s err=%s", got.Status, got.Error)
	}
	var rep agent.Report
	if err := json.Unmarshal([]byte(got.ReportJSON), &rep); err != nil {
		t.Fatalf("报告解析失败: %v", err)
	}
	if rep.Confidence == "low" {
		t.Fatalf("带建议的任务置信度不应为 low，报告=%+v", rep)
	}
	// 事实不应重复：同一条事实只出现一次。
	seen := map[string]bool{}
	for _, f := range rep.Facts {
		if seen[f] {
			t.Fatalf("事实重复出现: %q，全部事实=%v", f, rep.Facts)
		}
		seen[f] = true
	}
	steps, _ := s.ListSteps(ctx, r.ID)
	if len(steps) > 2 {
		t.Fatalf("带建议的任务不应重跑 Agent 循环，步骤数应 <=2，得到 %d", len(steps))
	}
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil || proposal.Status != store.ChangePending {
		t.Fatalf("应生成待审批提案: %v %+v", err, proposal)
	}
	if proposal.OldValue != "demo-api:bad-tag" || proposal.NewValue != "demo-api:latest" {
		t.Fatalf("提案新旧值不对: %+v", proposal)
	}
	if len(proposal.Patches) != 1 || !strings.Contains(proposal.Patches[0].Path, "/image") {
		t.Fatalf("提案应是镜像 Patch: %+v", proposal.Patches)
	}
}

// TestWorkerCarriedCreationGeneratesProposal 覆盖带创建建议的任务：
// Worker 不重新诊断，直接校验、生成创建提案；
// 批准后执行创建、验证 rollout 通过，集群里真的多了一个 Deployment。
func TestWorkerCarriedCreationGeneratesProposal(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	fake := kube.NewFake()
	tools := &ToolsAdapter{Kube: fake}
	w := NewWorker(s, tools, model.NewStub())
	exec := repair.NewExecutor(s, fake)
	// 测试里把验证超时和轮询间隔调小，不然 120 秒的用例跑不完。
	exec.VerifyTimeout = 5 * time.Second
	exec.VerifyInterval = 20 * time.Millisecond
	exec.PatchTimeout = 5 * time.Second
	w.SetRepair(exec)

	creationJSON := `{"namespace":"demo","name":"web-demo","image":"nginx:1.25","replicas":2,"reason":"用户要求创建 Deployment web-demo"}`
	r := &store.Run{
		Namespace: "demo", Kind: "Deployment", Name: "web-demo",
		Question: "创建 Deployment web-demo", MaxSteps: 8, TimeoutSeconds: 60,
		CreationJSON: creationJSON,
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if ran, err := w.RunOne(ctx); err != nil || !ran {
		t.Fatalf("Worker 应领到任务: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Status != store.StatusCompleted {
		t.Fatalf("带创建建议的任务应完成，状态=%s err=%s", got.Status, got.Error)
	}
	proposal, err := s.GetChangeRequestByRun(ctx, r.ID)
	if err != nil || proposal == nil || proposal.Status != store.ChangePending {
		t.Fatalf("应生成待审批创建提案: %v %+v", err, proposal)
	}
	if proposal.Kind != "create" || proposal.Creation == nil || proposal.Creation.Name != "web-demo" {
		t.Fatalf("提案应为创建类型: %+v", proposal)
	}
	if _, _, err := s.DecideApproval(ctx, proposal.ID, store.ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准提案失败: %v", err)
	}
	if ran, err := w.ProcessOneRepair(ctx); err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	done, _ := s.GetChangeRequest(ctx, proposal.ID)
	if done.ExecutionStatus != store.ExecExecuted || done.VerificationStatus != store.VerifyPassed {
		t.Fatalf("创建执行+验证应通过，得到 %s / %s，详情 %s", done.ExecutionStatus, done.VerificationStatus, done.ExecutionDetail)
	}
	dep, err := fake.GetDeployment(ctx, "demo", "web-demo")
	if err != nil {
		t.Fatalf("创建后应能读到 Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "nginx:1.25" {
		t.Fatalf("镜像应为 nginx:1.25，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
}
