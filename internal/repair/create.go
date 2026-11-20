// 创建 Deployment 的校验、构造和执行（Slice 10）。
//
// 设计上创建和 Patch 是同一条线：建议卡 → 创建可审批任务 →
// 人工审批 → Worker 执行 → 验证 rollout，不为创建单独写一套
// 审批 / 执行流程。区别只在执行动作本身：
// Patch 提案调 RepairClient.PatchDeployment，
// 创建提案调 RepairClient.CreateDeployment，验证共用同一套 Verify。
// 以后想改成 manager agent 时，创建能力可以直接复用，
// 再加别的操作（删、扩缩容）也按这个模式加一种提案类型，
// 而不是每个操作单独写一套流程。
//
// 安全边界和 Patch 同一套思路，分三层：
//  1. 静态校验（ValidateCreation）：只允许创建 Deployment
//     （不直接创建裸 Pod）、系统命名空间拒绝、镜像名合法、
//     副本数 1-10、内存/CPU 在上限内；
//  2. 执行前校验（executeCreate）：目标不存在才能创建，
//     已存在且配置一致视为已创建（幂等），已存在且配置不一致标过期；
//  3. 执行和验证分开：Create 成功不等于创建成功，
//     验证要独立检查 rollout、新 Pod 是否 Ready，超时要明确标出来。
package repair

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubecause/internal/store"
)

// isSystemNamespaceForWrite 是写操作的系统命名空间禁区：
// 读系统命名空间是允许的（只读），但创建、Patch 都不能动它们。
// 和 ValidatePatch 里的禁区是同一份名单，两边改要一起改。
func isSystemNamespaceForWrite(ns string) bool {
	return ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease"
}

// NormalizeCreation 给创建规格补默认值，不改原对象，返回一份新规格：
// 副本数 0 表示没指定，默认 1；容器名空时默认 = Deployment 名
// （单容器 Deployment 的常规情况，和 demo-api 的做法一致）。
// 校验之前先调它，调用方不用每个地方都写一遍默认值。
func NormalizeCreation(spec *store.CreationSpec) *store.CreationSpec {
	if spec == nil {
		return nil
	}
	out := *spec
	if out.Replicas <= 0 {
		out.Replicas = 1
	}
	if strings.TrimSpace(out.Container) == "" {
		out.Container = out.Name
	}
	return &out
}

// ValidateCreation 是创建提案的静态校验，第一道防线。
// 只按字段分，不按场景分：对话里创建、单次诊断里创建、
// 以后 manager agent 里创建，都过同一套检查。
// 规则：
//   - 目标必须是合法的 namespace / Deployment 名；
//   - 系统命名空间一律拒绝；
//   - 镜像名必须合法（和 Patch 的镜像校验同一条正则）；
//   - 副本数 1-10；
//   - 内存 limit/request 有值时必须是 Mi/Gi 结尾，
//     且不超过 repair.maxMemoryMi（默认 4Gi，配置中心可调）；
//   - CPU limit/request 有值时必须是合法数量、且大于 0；
//   - request 不超过同类 limit（都给了时才比）。
//
// 和集群现状有关的检查（目标是否已存在）不在这里，
// 在提案生成（Worker）和执行前（executeCreate）各查一次现状。
func ValidateCreation(spec *store.CreationSpec) error {
	if spec == nil {
		return fmt.Errorf("创建规格为空")
	}
	if spec.Namespace == "" || spec.Name == "" || spec.Image == "" {
		return fmt.Errorf("创建缺少目标信息：namespace、名字、镜像缺一不可")
	}
	if isSystemNamespaceForWrite(spec.Namespace) {
		return fmt.Errorf("禁止在系统命名空间 %q 里创建", spec.Namespace)
	}
	if !repairNsRe.MatchString(spec.Namespace) {
		return fmt.Errorf("namespace 格式非法: %q", spec.Namespace)
	}
	if !repairNameRe.MatchString(spec.Name) {
		return fmt.Errorf("Deployment 名格式非法: %q", spec.Name)
	}
	if strings.Contains(spec.Image, " ") || !imageRe.MatchString(spec.Image) {
		return fmt.Errorf("镜像名非法: %q", spec.Image)
	}
	if spec.Container != "" && !repairNameRe.MatchString(spec.Container) {
		return fmt.Errorf("容器名格式非法: %q", spec.Container)
	}
	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	if replicas < 1 || replicas > 10 {
		return fmt.Errorf("副本数必须是 1-10 之间的整数，得到 %d", spec.Replicas)
	}
	checkMemory := func(field, v string) error {
		if v == "" {
			return nil
		}
		if !isMemoryValue(v) {
			return fmt.Errorf("%s 必须是 Mi 或 Gi 结尾，例如 128Mi、2Gi，得到 %q", field, v)
		}
		mi, err := parseMemoryMi(v)
		if err != nil || mi <= 0 {
			return fmt.Errorf("%s 解析失败: %q", field, v)
		}
		if mi > MaxMemoryMi {
			return fmt.Errorf("%s %s 超过上限 %dMi，拒绝创建", field, v, MaxMemoryMi)
		}
		return nil
	}
	if err := checkMemory("内存 limit", spec.MemoryLimit); err != nil {
		return err
	}
	if err := checkMemory("内存 request", spec.MemoryRequest); err != nil {
		return err
	}
	if spec.MemoryLimit != "" && spec.MemoryRequest != "" {
		limitMi, _ := parseMemoryMi(spec.MemoryLimit)
		reqMi, _ := parseMemoryMi(spec.MemoryRequest)
		if reqMi > limitMi {
			return fmt.Errorf("内存 request %s 不能大于 limit %s", spec.MemoryRequest, spec.MemoryLimit)
		}
	}
	checkCPU := func(field, v string) error {
		if v == "" {
			return nil
		}
		q, err := resource.ParseQuantity(v)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("%s 非法: %q", field, v)
		}
		return nil
	}
	if err := checkCPU("CPU limit", spec.CpuLimit); err != nil {
		return err
	}
	if err := checkCPU("CPU request", spec.CpuRequest); err != nil {
		return err
	}
	return nil
}

// BuildDeployment 按创建规格拼一个 Deployment 对象。
// 只拼最常规的形状，不替用户做额外决定：
// 单容器、标签 app=<名字>、selector 按 app 标签选、
// 容器名默认 = Deployment 名、资源按规格给（没给就不填，
// 不编一个默认值）。调用前必须先过 ValidateCreation，
// 这里再按规格解析资源，解析失败返回错误，不静默用默认值。
func BuildDeployment(spec *store.CreationSpec) (*appsv1.Deployment, error) {
	spec = NormalizeCreation(spec)
	if err := ValidateCreation(spec); err != nil {
		return nil, err
	}
	replicas := int32(spec.Replicas)
	ct := corev1.Container{
		Name:  spec.Container,
		Image: spec.Image,
	}
	limits := corev1.ResourceList{}
	requests := corev1.ResourceList{}
	if spec.MemoryLimit != "" {
		limits[corev1.ResourceMemory] = resource.MustParse(spec.MemoryLimit)
	}
	if spec.CpuLimit != "" {
		limits[corev1.ResourceCPU] = resource.MustParse(spec.CpuLimit)
	}
	if spec.MemoryRequest != "" {
		requests[corev1.ResourceMemory] = resource.MustParse(spec.MemoryRequest)
	}
	if spec.CpuRequest != "" {
		requests[corev1.ResourceCPU] = resource.MustParse(spec.CpuRequest)
	}
	if len(limits) > 0 {
		ct.Resources.Limits = limits
	}
	if len(requests) > 0 {
		ct.Resources.Requests = requests
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels:    map[string]string{"app": spec.Name},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": spec.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": spec.Name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{ct}},
			},
		},
	}
	return dep, nil
}

// creationMatches 检查已存在的 Deployment 是否和创建规格一致：
// 容器镜像、副本数、以及规格里给了的资源值都对得上。
// 一致时执行可以记为“已创建”（幂等，上次执行后进程崩了、
// 重启后重跑不会重复创建）；不一致时说明同名目标已被别人改过，
// 提案的前提变了，标过期。比较内存/CPU 按数量比较，
// "256Mi" 和 "256Mi" 相等，"0.5" 和 "500m" 也相等。
func creationMatches(existing *appsv1.Deployment, spec *store.CreationSpec) bool {
	if existing == nil || spec == nil {
		return false
	}
	if len(existing.Spec.Template.Spec.Containers) == 0 {
		return false
	}
	ct := existing.Spec.Template.Spec.Containers[0]
	if spec.Container != "" {
		found := false
		for _, c := range existing.Spec.Template.Spec.Containers {
			if c.Name == spec.Container {
				ct = c
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if ct.Image != spec.Image {
		return false
	}
	want := int32(spec.Replicas)
	if want == 0 {
		want = 1
	}
	got := int32(1)
	if existing.Spec.Replicas != nil {
		got = *existing.Spec.Replicas
	}
	if got != want {
		return false
	}
	quantityEqual := func(field string, want string, got resource.Quantity) bool {
		if want == "" {
			return true
		}
		wq, err := resource.ParseQuantity(want)
		if err != nil {
			return false
		}
		return wq.Cmp(got) == 0
	}
	if !quantityEqual("memoryLimit", spec.MemoryLimit, *ct.Resources.Limits.Memory()) {
		return false
	}
	if !quantityEqual("memoryRequest", spec.MemoryRequest, *ct.Resources.Requests.Memory()) {
		return false
	}
	if !quantityEqual("cpuLimit", spec.CpuLimit, *ct.Resources.Limits.Cpu()) {
		return false
	}
	if !quantityEqual("cpuRequest", spec.CpuRequest, *ct.Resources.Requests.Cpu()) {
		return false
	}
	return true
}

// executeCreate 做创建提案的执行：dry-run 先行，再真实创建，最后读回确认。
// 流程和 executePatch 对称，只是动作从 Patch 换成 Create：
//  1. 目标已存在时先看配置是否一致：一致记为已创建（幂等），
//     不一致标过期，不覆盖别人的 Deployment；
//  2. dry-run 只校验，不落盘，失败就停在这里；
//  3. 真实 Create；写入出错时先读回目标再下结论，
//     创建可能其实已经生效了，只是响应没回来；
//  4. 读回确认目标存在、配置一致才记 executed，否则记 unknown，
//     不谎报成功。
//
// 验证（rollout、新 Pod Ready）不在这里，在 Verify 里共用同一套逻辑。
func (e *Executor) executeCreate(ctx context.Context, c *store.ChangeRequest) *ExecutionResult {
	spec := NormalizeCreation(c.Creation)
	dep, err := BuildDeployment(spec)
	if err != nil {
		return &ExecutionResult{Status: store.ExecFailed, Kind: "create", Error: "构造 Deployment 失败：" + err.Error()}
	}
	// 1. 目标已存在时先看配置：一致就是幂等（之前已创建过），
	//    不一致说明同名目标已被别人占用或改过，标过期。
	if existing, err := e.Repair.GetDeploymentForRepair(ctx, spec.Namespace, spec.Name); err == nil && existing != nil {
		if creationMatches(existing, spec) {
			return &ExecutionResult{Status: store.ExecExecuted, Kind: "create", UID: string(existing.UID), ResourceVersion: existing.ResourceVersion, AfterValue: spec.Image, Satisfied: true, AlreadyApplied: true, OldPodTemplateHash: "", PatchTime: time.Now()}
		}
		reason := fmt.Sprintf("同名 Deployment %s/%s 已存在，且配置和创建建议不一致", spec.Namespace, spec.Name)
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, Kind: "create", Error: reason}
	}

	// 2. dry-run 先行：只校验，不落盘。
	createCtx, cancel := context.WithTimeout(ctx, e.patchTimeout())
	defer cancel()
	if _, err := e.Repair.CreateDeployment(createCtx, dep, true); err != nil {
		return &ExecutionResult{Status: store.ExecFailed, Kind: "create", Error: "dry-run 失败：" + err.Error()}
	}

	// 3. 真实 Create。
	created, err := e.Repair.CreateDeployment(createCtx, dep, false)
	if err != nil {
		// 写入出错先读回目标再下结论：Create 可能其实已经生效了，
		// 只是响应没回来，和 Patch 的 readBackAfterPatchError 同一套思路。
		if existing, err2 := e.Repair.GetDeploymentForRepair(ctx, spec.Namespace, spec.Name); err2 == nil && creationMatches(existing, spec) {
			return &ExecutionResult{Status: store.ExecExecuted, Kind: "create", UID: string(existing.UID), ResourceVersion: existing.ResourceVersion, AfterValue: spec.Image, Satisfied: true, DryRunOK: true, OldPodTemplateHash: "", PatchTime: time.Now(), Error: fmt.Sprintf("Create 返回错误（%v），但读回确认已创建", err)}
		}
		return &ExecutionResult{Status: store.ExecFailed, Kind: "create", DryRunOK: true, Error: "创建失败：" + err.Error()}
	}

	// 4. 写入后读回确认：目标存在、配置一致才记 executed。
	if existing, err := e.Repair.GetDeploymentForRepair(ctx, spec.Namespace, spec.Name); err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, Kind: "create", UID: string(created.UID), DryRunOK: true, Error: "写入后读回目标失败，无法确认是否生效：" + err.Error()}
	} else if !creationMatches(existing, spec) {
		return &ExecutionResult{Status: store.ExecUnknown, Kind: "create", UID: string(created.UID), DryRunOK: true, Error: "写入后读回的配置和创建建议不一致，无法确认是否生效"}
	}
	return &ExecutionResult{Status: store.ExecExecuted, Kind: "create", UID: string(created.UID), ResourceVersion: created.ResourceVersion, AfterValue: spec.Image, Satisfied: true, DryRunOK: true, OldPodTemplateHash: "", PatchTime: time.Now()}
}
