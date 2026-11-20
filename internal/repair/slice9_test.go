package repair

import (
	"context"
	"strings"
	"testing"
	"time"

	"kubecause/internal/kube"
	"kubecause/internal/store"
)

// TestValidatePatchWhitelistGeneric 覆盖 Slice 9 的通用 Patch 白名单：
// 校验只按字段分，不按故障分。镜像、内存、副本数、CPU 都能过；
// 黑名单字段、系统命名空间、副本数超限一律拒绝。
func TestValidatePatchWhitelistGeneric(t *testing.T) {
	image := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "nginx:1.25"}}
	if err := ValidatePatch("demo", "demo-api", image); err != nil {
		t.Fatalf("镜像 Patch 应通过: %v", err)
	}
	mem := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256Mi"}}
	if err := ValidatePatch("demo", "demo-api", mem); err != nil {
		t.Fatalf("内存 Patch 应通过: %v", err)
	}
	replicas := []store.PatchOperation{{Op: "replace", Path: "/spec/replicas", Value: float64(3)}}
	if err := ValidatePatch("demo", "demo-api", replicas); err != nil {
		t.Fatalf("副本数 Patch 应通过: %v", err)
	}
	cpu := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/cpu", Value: "500m"}}
	if err := ValidatePatch("demo", "demo-api", cpu); err != nil {
		t.Fatalf("CPU Patch 应通过: %v", err)
	}
	// 黑名单：hostNetwork、privileged、command、metadata/name 都拒绝。
	for _, path := range []string{
		"/spec/template/spec/hostNetwork",
		"/spec/template/spec/containers/0/securityContext/privileged",
		"/spec/template/spec/containers/0/command",
		"/spec/template/spec/containers/0/args",
		"/metadata/name",
		"/metadata/namespace",
	} {
		bad := []store.PatchOperation{{Op: "replace", Path: path, Value: "x"}}
		if err := ValidatePatch("demo", "demo-api", bad); err == nil {
			t.Fatalf("黑名单 path 应被拒绝: %s", path)
		}
	}
	// 系统命名空间拒绝。
	if err := ValidatePatch("kube-system", "coredns", image); err == nil {
		t.Fatalf("系统命名空间应被拒绝")
	}
	// 副本数超过 10 拒绝。
	tooMany := []store.PatchOperation{{Op: "replace", Path: "/spec/replicas", Value: float64(11)}}
	if err := ValidatePatch("demo", "demo-api", tooMany); err == nil {
		t.Fatalf("副本数 11 应被拒绝")
	}
	// op 不是 replace 拒绝。
	add := []store.PatchOperation{{Op: "add", Path: "/spec/replicas", Value: float64(2)}}
	if err := ValidatePatch("demo", "demo-api", add); err == nil {
		t.Fatalf("op=add 应被拒绝")
	}
	// 镜像名非法拒绝：空、带空格。
	for _, img := range []string{"", "nginx:1.25 beta", "NGINX:1.25"} {
		bad := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: img}}
		if err := ValidatePatch("demo", "demo-api", bad); err == nil {
			t.Fatalf("非法镜像名应被拒绝: %q", img)
		}
	}
	// 内存值格式不对拒绝。
	badMem := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256"}}
	if err := ValidatePatch("demo", "demo-api", badMem); err == nil {
		t.Fatalf("非 Mi/Gi 内存值应被拒绝")
	}
	// 空 Patch 列表拒绝。
	if err := ValidatePatch("demo", "demo-api", nil); err == nil {
		t.Fatalf("空 Patch 列表应被拒绝")
	}
}

// TestValidatePatchWithCurrentMemoryRatio 覆盖现状相关的内存校验：
// 新值相对现状的倍数和上限，现状读不到时不做这层检查。
func TestValidatePatchWithCurrentMemoryRatio(t *testing.T) {
	ctx := context.Background()
	f := kube.NewFake()
	dep, err := f.GetDeployment(ctx, "demo", "demo-api")
	if err != nil {
		t.Fatalf("读 Deployment 失败: %v", err)
	}
	// 128Mi -> 256Mi（2 倍）通过；128Mi -> 1Gi（8 倍）拒绝。
	ok := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256Mi"}}
	if err := ValidatePatchWithCurrent(dep, ok); err != nil {
		t.Fatalf("2 倍应通过: %v", err)
	}
	tooBig := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "1Gi"}}
	if err := ValidatePatchWithCurrent(dep, tooBig); err == nil {
		t.Fatalf("8 倍应被拒绝")
	}
	// 容器下标越界拒绝。
	oob := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/5/image", Value: "nginx:1.25"}}
	if err := ValidatePatchWithCurrent(dep, oob); err == nil {
		t.Fatalf("容器下标越界应被拒绝")
	}
}

// makeApprovedPatch 创建一个任务和一份已批准的通用 Patch 提案。
func makeApprovedPatch(t *testing.T, ctx context.Context, s *store.Store, patches []store.PatchOperation, oldValue, newValue string) *store.ChangeRequest {
	t.Helper()
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "测试通用 Patch"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: oldValue, NewValue: newValue, Patches: patches, DeploymentUID: "uid-demo-api-deploy"})
	if err != nil {
		t.Fatalf("创建通用提案失败: %v", err)
	}
	if _, _, err := s.DecideApproval(ctx, p.ID, store.ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准提案失败: %v", err)
	}
	return p
}

// TestExecuteGenericImagePatch 覆盖通用执行改镜像：
// fake 上 Patch 镜像成功，读回确认新镜像已生效，rollout 验证通过。
func TestExecuteGenericImagePatch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFakeWithScenario("imagepull")
	e := newExecutor(t, s, f)
	patches := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/image", Value: "demo-api:latest"}}
	p := makeApprovedPatch(t, ctx, s, patches, "demo-api:bad-tag", "demo-api:latest")
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
	dep, _ := f.GetDeployment(ctx, "demo", "demo-api")
	if dep.Spec.Template.Spec.Containers[0].Image != "demo-api:latest" {
		t.Fatalf("写入后读回的镜像应为 demo-api:latest，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
	if !strings.Contains(got.ExecutionDetail, "demo-api:latest") {
		t.Fatalf("执行结果里应有新镜像: %s", got.ExecutionDetail)
	}
}

// TestExecuteGenericReplicasPatch 覆盖通用执行改副本数：
// 1 -> 3，读回确认，rollout 验证通过。
func TestExecuteGenericReplicasPatch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	patches := []store.PatchOperation{{Op: "replace", Path: "/spec/replicas", Value: float64(3)}}
	p := makeApprovedPatch(t, ctx, s, patches, "1", "3")
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
	dep, _ := f.GetDeployment(ctx, "demo", "demo-api")
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Fatalf("写入后读回的副本数应为 3，得到 %v", dep.Spec.Replicas)
	}
}

// TestExecuteGenericMemoryPatch 覆盖通用执行改内存（新格式）：
// 用 Patches 而不用 old/new 专属路径，128Mi -> 256Mi 仍能跑通。
func TestExecuteGenericMemoryPatch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	patches := []store.PatchOperation{{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256Mi"}}
	p := makeApprovedPatch(t, ctx, s, patches, "128Mi", "256Mi")
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.ExecutionStatus != store.ExecExecuted || got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("执行+验证应通过，得到 %s / %s", got.ExecutionStatus, got.VerificationStatus)
	}
	if v := deploymentLimit(t, f); v != "256Mi" {
		t.Fatalf("写入后读回的 limit 应为 256Mi，得到 %s", v)
	}
}

// TestExecuteGenericMultiPatch 覆盖一份提案改多个字段：
// 内存 + 副本数同一批 Patch，一次 dry-run、一次写入、逐条读回。
func TestExecuteGenericMultiPatch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := kube.NewFake()
	e := newExecutor(t, s, f)
	e.VerifyTimeout = 5 * time.Second
	e.VerifyInterval = 20 * time.Millisecond
	patches := []store.PatchOperation{
		{Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: "256Mi"},
		{Op: "replace", Path: "/spec/replicas", Value: float64(2)},
	}
	p := makeApprovedPatch(t, ctx, s, patches, "128Mi", "256Mi")
	if ran, err := e.ExecuteApproved(ctx); err != nil || !ran {
		t.Fatalf("应执行一份已批准提案: ran=%v err=%v", ran, err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if got.ExecutionStatus != store.ExecExecuted || got.VerificationStatus != store.VerifyPassed {
		t.Fatalf("多 Patch 执行+验证应通过，得到 %s / %s，详情 %s", got.ExecutionStatus, got.VerificationStatus, got.ExecutionDetail)
	}
	dep, _ := f.GetDeployment(ctx, "demo", "demo-api")
	if dep.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String() != "256Mi" {
		t.Fatalf("内存应为 256Mi")
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("副本数应为 2")
	}
}

// TestCreateProposalLegacyStillWorks 覆盖向后兼容：
// 只给 old/new 值的老调用方式仍能创建提案，读出来是通用 Patch 格式。
func TestCreateProposalLegacyStillWorks(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "老提案"}
	_ = s.CreateRun(ctx, r)
	p, err := CreateProposal(s, &Proposal{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi", DeploymentUID: "uid-demo-api-deploy"})
	if err != nil {
		t.Fatalf("老方式创建提案应成功: %v", err)
	}
	got, _ := s.GetChangeRequest(ctx, p.ID)
	if len(got.Patches) != 1 || got.Patches[0].Path != "/spec/template/spec/containers/0/resources/limits/memory" {
		t.Fatalf("老提案读出来应是通用 Patch 格式，得到 %+v", got.Patches)
	}
	if got.Patches[0].Value != "256Mi" {
		t.Fatalf("老提案的 Patch 值应为 256Mi，得到 %v", got.Patches[0].Value)
	}
}
