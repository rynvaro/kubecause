package repair

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubecause/internal/kube"
	"kubecause/internal/store"
)

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newExecutor(t *testing.T, s *store.Store, rc kube.RepairClient) *Executor {
	t.Helper()
	e := NewExecutor(s, rc)
	// 测试里把验证超时和轮询间隔调小，不然 120 秒的用例跑不完。
	e.VerifyTimeout = 5 * time.Second
	e.VerifyInterval = 20 * time.Millisecond
	e.PatchTimeout = 5 * time.Second
	return e
}

// makeApproved 创建一个任务和一份已批准的提案，返回提案。
// uid 为空表示不绑定 UID（老提案），否则绑定 fake 的 Deployment UID。
func makeApproved(t *testing.T, ctx context.Context, s *store.Store, uid, oldValue string) *store.ChangeRequest {
	t.Helper()
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: oldValue, NewValue: "256Mi", DeploymentUID: uid})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	if _, _, err := s.DecideApproval(ctx, p.ID, store.ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准提案失败: %v", err)
	}
	return p
}

func deploymentLimit(t *testing.T, f *kube.Fake) string {
	t.Helper()
	dep, err := f.GetDeployment(context.Background(), "demo", "demo-api")
	if err != nil {
		t.Fatalf("读 Deployment 失败: %v", err)
	}
	return dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String()
}

func TestValidatePatchSpecWhitelist(t *testing.T) {
	// 合法的只有 demo/demo-api/demo-api 这一组。
	if err := ValidatePatchSpec("demo", "demo-api", "demo-api", "128Mi", "256Mi"); err != nil {
		t.Fatalf("合法提案应通过白名单: %v", err)
	}
	// 非法目标一律拒绝：系统 namespace、别的 Deployment、别的容器。
	for _, tc := range []struct{ ns, dep, ct string }{
		{"kube-system", "demo-api", "demo-api"},
		{"demo", "other-api", "demo-api"},
		{"demo", "demo-api", "sidecar"},
		{"", "demo-api", "demo-api"},
	} {
		if err := ValidatePatchSpec(tc.ns, tc.dep, tc.ct, "128Mi", "256Mi"); err == nil {
			t.Fatalf("非法目标应被白名单拒绝: %+v", tc)
		}
	}
	// 非法值也拒绝：不是 Mi 结尾、新旧值相同。
	if err := ValidatePatchSpec("demo", "demo-api", "demo-api", "128", "256Mi"); err == nil {
		t.Fatalf("非 Mi 值应被拒绝")
	}
	if err := ValidatePatchSpec("demo", "demo-api", "demo-api", "256Mi", "256Mi"); err == nil {
		t.Fatalf("新旧值相同应被拒绝")
	}
}

func TestExecuteSuccessFake(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	p := makeApproved(t, ctx, s, "uid-demo-api-deploy", "128Mi")
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.ExecutionStatus != store.ExecExecuted {
		t.Fatalf("执行状态应为 executed，得到 %s，详情 %s", got.ExecutionStatus, got.ExecutionDetail)
	}
	if got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("验证状态应为 passed，得到 %s，详情 %s", got.VerificationStatus, got.VerificationDetail)
	}
	if !strings.Contains(got.ExecutionDetail, "256Mi") {
		t.Fatalf("执行结果里应有新值 256Mi: %s", got.ExecutionDetail)
	}
	if !strings.Contains(got.VerificationDetail, "passed") {
		t.Fatalf("验证结果里应有 passed: %s", got.VerificationDetail)
	}
	// 写入后读回确认：Deployment 的 limit 真的变了。
	if v := deploymentLimit(t, f); v != "256Mi" {
		t.Fatalf("写入后读回的 limit 应为 256Mi，得到 %s", v)
	}
	// 时间线里应有 repair 步骤，执行和验证分开记。
	steps, _ := s.ListSteps(ctx, p.RunID)
	foundExec, foundVerify := false, false
	for _, st := range steps {
		if st.Kind == "repair" && strings.Contains(st.Summary, "修复已写入") {
			foundExec = true
		}
		if st.Kind == "repair" && strings.Contains(st.Summary, "验证通过") {
			foundVerify = true
		}
	}
	if !foundExec || !foundVerify {
		t.Fatalf("时间线里应有执行和验证两步: %+v", steps)
	}
}

func TestExecuteDryRunDoesNotChange(t *testing.T) {
	ctx := context.Background()
	f := kube.NewFake()
	// dry-run 只校验不落盘。
	if _, err := f.PatchDeploymentMemoryLimit(ctx, "demo", "demo-api", "demo-api", "256Mi", "", true); err != nil {
		t.Fatalf("dry-run 应成功: %v", err)
	}
	if v := deploymentLimit(t, f); v != "128Mi" {
		t.Fatalf("dry-run 后 limit 应仍为 128Mi，得到 %s", v)
	}
}

func TestExecuteUIDMismatchExpired(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	// 提案绑定的 UID 和现在的不一样，目标可能已被重建，标过期，不执行。
	p := makeApproved(t, ctx, s, "uid-someone-else", "128Mi")
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || !ran {
		t.Fatalf("应领到提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.Status != store.ChangeExpired || got.ExecutionStatus != store.ExecExpired {
		t.Fatalf("UID 不匹配应标为过期: status=%s execution=%s", got.Status, got.ExecutionStatus)
	}
	if v := deploymentLimit(t, f); v != "128Mi" {
		t.Fatalf("过期后不应写入，limit 应仍为 128Mi，得到 %s", v)
	}
}

func TestExecuteOldValueMismatchExpired(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	// 提案旧值 64Mi，但当前是 128Mi，前提已变，标过期，不执行。
	p := makeApproved(t, ctx, s, "uid-demo-api-deploy", "64Mi")
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || !ran {
		t.Fatalf("应领到提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.Status != store.ChangeExpired {
		t.Fatalf("旧值不匹配应标为过期: %+v", got)
	}
	if v := deploymentLimit(t, f); v != "128Mi" {
		t.Fatalf("过期后不应写入，limit 应仍为 128Mi，得到 %s", v)
	}
}

func TestExecuteAlreadyAppliedIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	// 先把 limit 改成 256Mi，模拟上次执行后进程崩了、重启后重跑。
	if _, err := f.PatchDeploymentMemoryLimit(ctx, "demo", "demo-api", "demo-api", "256Mi", "", false); err != nil {
		t.Fatalf("预置新值失败: %v", err)
	}
	e := newExecutor(t, s, f)
	p := makeApproved(t, ctx, s, "uid-demo-api-deploy", "128Mi")
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || !ran {
		t.Fatalf("应领到提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.ExecutionStatus != store.ExecExecuted {
		t.Fatalf("当前值已是新值时应直接记为已执行，得到 %s", got.ExecutionStatus)
	}
	if !strings.Contains(got.ExecutionDetail, "alreadyApplied") {
		t.Fatalf("执行结果应标明已应用过: %s", got.ExecutionDetail)
	}
}

func TestExecuteRejectedNoWrite(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启"}
	_ = s.CreateRun(ctx, r)
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi", DeploymentUID: "uid-demo-api-deploy"})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	if _, _, err := s.DecideApproval(ctx, p.ID, store.ChangeRejected, "varo"); err != nil {
		t.Fatalf("拒绝提案失败: %v", err)
	}
	// 拒绝的提案永远不会被领到。
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || ran {
		t.Fatalf("拒绝后不应执行任何提案: ran=%v err=%v", ran, err)
	}
	if v := deploymentLimit(t, f); v != "128Mi" {
		t.Fatalf("拒绝后不应写入，limit 应仍为 128Mi，得到 %s", v)
	}
	// 直接调 Execute 也应拒绝执行。
	if _, _, err := e.Execute(ctx, p.ID); err == nil {
		t.Fatalf("拒绝的提案直接 Execute 也应报错")
	}
}

// restartGrowingClient 包装一个正常的 RepairClient，
// 每次列新 Pod 时都让重启次数 +1，模拟“修复后 Pod 还在重启”的情况。
// 验证应发现重启次数继续增长，直接判失败，不用等到超时。
type restartGrowingClient struct {
	kube.RepairClient
	calls int
}

func (g *restartGrowingClient) ListPodsForRepair(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	pods, err := g.RepairClient.ListPodsForRepair(ctx, namespace, labels)
	if err != nil {
		return nil, err
	}
	g.calls++
	for i := range pods {
		if pods[i].Labels["pod-template-hash"] == "9a1b2c3d4" {
			// 新 Pod 的重启次数随每次轮询增长。
			for j := range pods[i].Status.ContainerStatuses {
				pods[i].Status.ContainerStatuses[j].RestartCount = int32(g.calls - 1)
			}
		}
	}
	return pods, nil
}

func TestVerifyRestartGrowthFails(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	// 先正常 Patch 一次，让 Fake 模拟出新 Pod。
	if _, err := f.PatchDeploymentMemoryLimit(ctx, "demo", "demo-api", "demo-api", "256Mi", "", false); err != nil {
		t.Fatalf("预置 Patch 失败: %v", err)
	}
	growing := &restartGrowingClient{RepairClient: f}
	e := newExecutor(t, s, growing)
	// 用一个已批准提案的壳调 Verify，只测验证逻辑本身。
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "demo-api 为什么一直重启"}
	_ = s.CreateRun(ctx, r)
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi", DeploymentUID: "uid-demo-api-deploy"})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	execRes := &ExecutionResult{Status: store.ExecExecuted, OldPodTemplateHash: "7d9f6c4b5", PatchTime: time.Now()}
	verRes := e.Verify(ctx, p, execRes)
	if verRes.Status != store.VerifyFailed {
		t.Fatalf("重启次数继续增长时验证应失败，得到 %s %+v", verRes.Status, verRes)
	}
	if !verRes.RestartsGrew {
		t.Fatalf("验证结果应标明重启次数增长了: %+v", verRes)
	}
}

func TestVerifyNewPodReadyPasses(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	p := makeApproved(t, ctx, s, "uid-demo-api-deploy", "128Mi")
	ran, err := e.ExecuteApproved(ctx)
	if err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("新 Pod Ready 时验证应通过，得到 %s，详情 %s", got.VerificationStatus, got.VerificationDetail)
	}
	if !strings.Contains(got.VerificationDetail, "9a1b2c3d4") && !strings.Contains(got.VerificationDetail, "demo-api-9a1b2c3d4") {
		// 验证详情里应有新 Pod 名（来自 Fake 模拟的新 Pod）。
		t.Fatalf("验证结果里应有新 Pod 名: %s", got.VerificationDetail)
	}
}
