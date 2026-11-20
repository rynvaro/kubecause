package repair

import (
	"context"
	"strings"
	"testing"

	"kubecause/internal/kube"
	"kubecause/internal/store"
)

// TestValidateCreationRejects 覆盖创建校验的拒绝项：
// 系统命名空间、副本数超限、镜像非法、内存超上限、
// request 大于 limit、CPU 非法，缺一不可，
// 校验不通过就不生成提案。
func TestValidateCreationRejects(t *testing.T) {
	cases := []struct {
		name string
		spec *store.CreationSpec
	}{
		{"系统命名空间", &store.CreationSpec{Namespace: "kube-system", Name: "web", Image: "nginx:1.25"}},
		{"副本数超限", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx:1.25", Replicas: 11}},
		{"镜像含空格", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx 1.25"}},
		{"镜像大写开头", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "NGINX:1.25"}},
		{"镜像为空", &store.CreationSpec{Namespace: "demo", Name: "web", Image: ""}},
		{"名字为空", &store.CreationSpec{Namespace: "demo", Name: "", Image: "nginx:1.25"}},
		{"内存超上限", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx:1.25", MemoryLimit: "8192Mi"}},
		{"内存格式非法", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx:1.25", MemoryLimit: "256M"}},
		{"request 大于 limit", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx:1.25", MemoryLimit: "128Mi", MemoryRequest: "256Mi"}},
		{"CPU 非法", &store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx:1.25", CpuLimit: "abc"}},
	}
	for _, tc := range cases {
		if err := ValidateCreation(tc.spec); err == nil {
			t.Fatalf("%s 应被拒绝，却通过了: %+v", tc.name, tc.spec)
		}
	}
}

// TestValidateCreationAccepts 覆盖创建校验的通过项：
// 完整规格（副本数、内存、CPU 都给）能过，
// 副本数 0 表示没指定，默认 1，也能过。
func TestValidateCreationAccepts(t *testing.T) {
	valid := &store.CreationSpec{
		Namespace: "demo", Name: "web", Image: "nginx:1.25", Replicas: 2,
		MemoryLimit: "256Mi", MemoryRequest: "64Mi", CpuLimit: "500m", CpuRequest: "100m",
	}
	if err := ValidateCreation(valid); err != nil {
		t.Fatalf("完整规格应通过: %v", err)
	}
	if err := ValidateCreation(&store.CreationSpec{Namespace: "demo", Name: "web", Image: "nginx"}); err != nil {
		t.Fatalf("只给必填项也应通过: %v", err)
	}
}

// TestBuildDeployment 覆盖按规格拼 Deployment：
// 标签和 selector 都是 app=<名字>，容器名默认 = Deployment 名，
// 镜像、副本数、资源按规格给，没给资源时不编默认值。
func TestBuildDeployment(t *testing.T) {
	dep, err := BuildDeployment(&store.CreationSpec{
		Namespace: "demo", Name: "web", Image: "nginx:1.25", Replicas: 2,
		MemoryLimit: "256Mi", MemoryRequest: "64Mi",
	})
	if err != nil {
		t.Fatalf("构造 Deployment 失败: %v", err)
	}
	if dep.Namespace != "demo" || dep.Name != "web" {
		t.Fatalf("目标不对: %s/%s", dep.Namespace, dep.Name)
	}
	if dep.Labels["app"] != "web" || dep.Spec.Selector.MatchLabels["app"] != "web" {
		t.Fatalf("标签/selector 应为 app=web，得到 %+v / %+v", dep.Labels, dep.Spec.Selector)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("副本数应为 2，得到 %v", dep.Spec.Replicas)
	}
	ct := dep.Spec.Template.Spec.Containers[0]
	if ct.Name != "web" || ct.Image != "nginx:1.25" {
		t.Fatalf("容器不对: %+v", ct)
	}
	if ct.Resources.Limits.Memory().String() != "256Mi" || ct.Resources.Requests.Memory().String() != "64Mi" {
		t.Fatalf("资源不对: %+v", ct.Resources)
	}
	if _, err := BuildDeployment(&store.CreationSpec{Namespace: "kube-system", Name: "web", Image: "nginx:1.25"}); err == nil {
		t.Fatalf("系统命名空间应构造失败")
	}
}

// makeApprovedCreation 创建一个任务和一份已批准的创建提案。
func makeApprovedCreation(t *testing.T, ctx context.Context, s *store.Store, spec *store.CreationSpec) *store.ChangeRequest {
	t.Helper()
	r := &store.Run{Namespace: spec.Namespace, Kind: "Deployment", Name: spec.Name, Question: "创建 Deployment " + spec.Name}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Creation: spec})
	if err != nil {
		t.Fatalf("创建创建提案失败: %v", err)
	}
	if p.Kind != "create" || p.Creation == nil || p.Creation.Name != spec.Name {
		t.Fatalf("提案应为创建类型: %+v", p)
	}
	if _, _, err := s.DecideApproval(ctx, p.ID, store.ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准提案失败: %v", err)
	}
	return p
}

// TestExecuteCreateFullFlow 覆盖创建全流程（Fake）：
// 提案 → 批准 → 执行创建 → 验证 rollout 通过，
// 集群里真的多了一个带 Ready Pod 的 Deployment。
func TestExecuteCreateFullFlow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	spec := &store.CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.25", Replicas: 2, MemoryLimit: "256Mi", MemoryRequest: "64Mi"}
	p := makeApprovedCreation(t, ctx, s, spec)
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.ExecutionStatus != store.ExecExecuted {
		t.Fatalf("执行状态应为 executed，得到 %s，详情 %s", got.ExecutionStatus, got.ExecutionDetail)
	}
	if got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("验证状态应为 passed，得到 %s，详情 %s", got.VerificationStatus, got.VerificationDetail)
	}
	if !strings.Contains(got.ExecutionDetail, "nginx:1.25") {
		t.Fatalf("执行结果里应有新镜像: %s", got.ExecutionDetail)
	}
	// 时间线里的执行摘要应说清楚创建了什么。
	steps, _ := s.ListSteps(ctx, p.RunID)
	found := false
	for _, st := range steps {
		if strings.Contains(st.Summary, "已创建 Deployment demo/web-demo") {
			found = true
		}
	}
	if !found {
		t.Fatalf("时间线里应有创建摘要，得到 %+v", steps)
	}
	dep, err := f.GetDeployment(ctx, "demo", "web-demo")
	if err != nil {
		t.Fatalf("创建后应能读到 Deployment: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "nginx:1.25" {
		t.Fatalf("镜像应为 nginx:1.25，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("副本数应为 2，得到 %v", dep.Spec.Replicas)
	}
	pods, err := f.ListPodsForRepair(ctx, "demo", map[string]string{"app": "web-demo"})
	if err != nil || len(pods) == 0 {
		t.Fatalf("创建后应有 Pod，得到 %d 个，err=%v", len(pods), err)
	}
	ready := false
	for _, cs := range pods[0].Status.ContainerStatuses {
		if cs.Ready {
			ready = true
		}
	}
	if !ready {
		t.Fatalf("新 Pod 应 Ready，得到 %+v", pods[0].Status)
	}
}

// TestExecuteCreateIdempotent 覆盖创建的幂等：
// 同一个规格再批一份创建提案并执行，目标已存在且配置一致，
// 直接记为已执行，不重复创建、不标过期。
func TestExecuteCreateIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	spec := &store.CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.25", Replicas: 1}
	p1 := makeApprovedCreation(t, ctx, s, spec)
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("第一次应执行: ran=%v err=%v", ran, err)
	}
	p2 := makeApprovedCreation(t, ctx, s, spec)
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("第二次应执行（幂等）: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p2.ID)
	if got.ExecutionStatus != store.ExecExecuted {
		t.Fatalf("第二次执行状态应为 executed，得到 %s，详情 %s", got.ExecutionStatus, got.ExecutionDetail)
	}
	if !strings.Contains(got.ExecutionDetail, "alreadyApplied") {
		t.Fatalf("第二次执行结果里应标 alreadyApplied: %s", got.ExecutionDetail)
	}
	_ = p1
}

// TestExecuteCreateConflictExpired 覆盖同名冲突：
// 目标已存在且配置不一致（镜像不同），创建提案执行时标过期，不覆盖别人的 Deployment。
func TestExecuteCreateConflictExpired(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	p1 := makeApprovedCreation(t, ctx, s, &store.CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.25"})
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("第一次应执行: ran=%v err=%v", ran, err)
	}
	p2 := makeApprovedCreation(t, ctx, s, &store.CreationSpec{Namespace: "demo", Name: "web-demo", Image: "nginx:1.26"})
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("第二次应执行（标过期）: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p2.ID)
	if got.ExecutionStatus != store.ExecExpired {
		t.Fatalf("冲突时执行状态应为 expired，得到 %s，详情 %s", got.ExecutionStatus, got.ExecutionDetail)
	}
	dep, _ := f.GetDeployment(ctx, "demo", "web-demo")
	if dep.Spec.Template.Spec.Containers[0].Image != "nginx:1.25" {
		t.Fatalf("冲突时不应覆盖已有 Deployment，镜像应仍为 nginx:1.25，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
	_ = p1
}

// TestCreateProposalCreationRejects 覆盖创建提案生成时的校验：
// 系统命名空间的创建规格不生成提案，直接报错。
func TestCreateProposalCreationRejects(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &store.Run{Namespace: "kube-system", Kind: "Deployment", Name: "web", Question: "创建"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := CreateProposal(s, &Proposal{RunID: r.ID, Creation: &store.CreationSpec{Namespace: "kube-system", Name: "web", Image: "nginx:1.25"}}); err == nil {
		t.Fatalf("系统命名空间的创建提案应被拒绝")
	}
}
