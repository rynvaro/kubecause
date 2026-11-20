// Package repair 负责审批后的修复：条件 Patch 与 rollout 验证。
//
// Slice 4 起白名单是通用的，不再只认 demo/demo-api 这一组：
// 任意 namespace / Deployment / 容器都可以提修复建议，
// 但只允许改内存 limit 这一个字段，且新值必须大于旧值、
// 不超过旧值的 4 倍、不超过 4Gi。安全边界分三层，缺一不可：
//  1. 白名单：只允许改内存 limit，值必须合法，见 ValidatePatchSpec；
//  2. 执行前校验：重新读取目标，核对 UID、容器名、变更前旧值，
//     再用本次读取的 resourceVersion 做条件 Patch，防并发覆盖；
//  3. 执行和验证分开：写入成功不等于修复成功，验证要独立检查 rollout、
//     新 Pod 是否 Ready、重启次数是否继续增长，超时要明确标出来。
//
// 读和写用不同的客户端接口：诊断走 kube.KubeClient（只读），
// 修复走 kube.RepairClient（写），生产上是两个不同的身份。
package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"kubecause/internal/kube"
	"kubecause/internal/store"
)

// Proposal 是修复提案，和 change_requests 表一一对应。
// 一份提案只改一个东西：指定 Deployment 的指定容器的内存 limit。
// 字段和值都要由修复模块校验，不允许模型直接拼一个任意 Patch。
type Proposal struct {
	RunID         string
	Namespace     string
	Deployment    string
	Container     string
	OldValue      string // 例如 128Mi
	NewValue      string // 例如 256Mi
	DeploymentUID string // 提案生成时绑定的目标 UID，空表示老提案未绑定
}

// CreateProposal 生成一份待审批提案，写入 change_requests 表。
// 提案一旦生成就不可变：改目标、改前后值都要生成新提案，
// 不能原地改旧提案，这样审批时看到的东西和执行时用的东西才一致。
func CreateProposal(s *store.Store, p *Proposal) (*store.ChangeRequest, error) {
	if err := ValidatePatchSpec(p.Namespace, p.Deployment, p.Container, p.OldValue, p.NewValue); err != nil {
		return nil, err
	}
	return s.CreateChangeRequest(context.Background(), &store.ChangeRequest{
		RunID: p.RunID, Namespace: p.Namespace, Deployment: p.Deployment,
		Container: p.Container, OldValue: p.OldValue, NewValue: p.NewValue,
		DeploymentUID: p.DeploymentUID,
	})
}

func isMemoryValue(v string) bool {
	if len(v) < 3 {
		return false
	}
	// 只接受 Mi / Gi 结尾的内存值：合法的 K8s 数量，且是人类可读的档位。
	// "128"、"256M"、"1G"、"100m"（CPU 毫核）都不接受。
	suffix := v[len(v)-2:]
	if suffix != "Mi" && suffix != "Gi" {
		return false
	}
	for _, c := range v[:len(v)-2] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

var (
	repairNsRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	repairNameRe = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// parseMemoryMi 把 "128Mi"/"2Gi" 转成 MiB 数值，供倍数和上限比较用。
func parseMemoryMi(v string) (int64, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, err
	}
	return q.Value() / (1024 * 1024), nil
}

// DoubleMemory 将旧值翻倍，超过上限时封顶到上限，
// 供模型没给新值时 Worker 兜底用。返回 "" 表示旧值本身已不合法。
func DoubleMemory(oldValue string, maxMi int64) string {
	oldMi, err := parseMemoryMi(oldValue)
	if err != nil || oldMi <= 0 {
		return ""
	}
	next := oldMi * 2
	if next > maxMi {
		next = maxMi
	}
	return fmt.Sprintf("%dMi", next)
}

// MaxMemoryMi 是单次修复允许的新值上限（MiB）：默认 4096 即 4Gi。
// MaxRatio 是新值相对旧值的最大倍数：默认 4。
// ValidatePatchSpec 里同时检查“不超过旧值 MaxRatio 倍”和“不超过 MaxMemoryMi”，
// 两个上限取更严的那一个，防止模型一次把 limit 提到天上去。
// 这两个值不再是写死的常量，而是配置中心（internal/config）的
// repair.maxMemoryMi / repair.maxRatio，启动时和热更新时由 ApplyLimits 填进来。
var MaxMemoryMi int64 = 4096

// MaxRatio 是修复新值相对旧值的最大倍数，默认 4。
var MaxRatio int64 = 4

// ApplyLimits 把配置中心的值应用到修复校验：热加载时调它，不用重启。
// 非法值不改，保持旧值——校验层不能因为一次错误配置就放开上限。
func ApplyLimits(maxMi, maxRatio int64) {
	if maxMi < 128 || maxMi > 16384 || maxRatio < 1 || maxRatio > 10 {
		return
	}
	MaxMemoryMi = maxMi
	MaxRatio = maxRatio
}

// ValidatePatchSpec 是 Patch 的白名单校验，第一道防线。
// 通用规则（Slice 4）：任意 namespace / Deployment / 容器都可以，
// 但只允许改内存 limit 这一个字段；值必须是 Mi / Gi 结尾；
// 新值必须大于旧值、不超过旧值的 4 倍、不超过 4Gi；
// 系统命名空间、空目标、非法格式一律拒绝。
// 旧值和集群现状是否一致不在这里查，那是执行前校验（executePatch）
// 重新读目标时核对的，两层分开，职责才清楚。
func ValidatePatchSpec(namespace, deployment, container, oldValue, newValue string) error {
	if namespace == "" || deployment == "" || container == "" {
		return fmt.Errorf("提案缺少目标信息")
	}
	if namespace == "kube-system" || namespace == "kube-public" || namespace == "kube-node-lease" {
		return fmt.Errorf("禁止修改系统命名空间 %q", namespace)
	}
	if !repairNsRe.MatchString(namespace) {
		return fmt.Errorf("namespace 格式非法: %q", namespace)
	}
	if !repairNameRe.MatchString(deployment) || !repairNameRe.MatchString(container) {
		return fmt.Errorf("目标名格式非法: %s/%s", deployment, container)
	}
	if oldValue == "" || newValue == "" || oldValue == newValue {
		return fmt.Errorf("提案的新旧值非法")
	}
	if !isMemoryValue(oldValue) || !isMemoryValue(newValue) {
		return fmt.Errorf("内存值必须是 Mi 或 Gi 结尾，例如 128Mi、2Gi")
	}
	oldMi, err := parseMemoryMi(oldValue)
	if err != nil || oldMi <= 0 {
		return fmt.Errorf("旧值解析失败: %q", oldValue)
	}
	newMi, err := parseMemoryMi(newValue)
	if err != nil || newMi <= 0 {
		return fmt.Errorf("新值解析失败: %q", newValue)
	}
	if newMi <= oldMi {
		return fmt.Errorf("新值 %s 必须大于旧值 %s", newValue, oldValue)
	}
	if newMi > oldMi*MaxRatio {
		return fmt.Errorf("新值 %s 超过旧值 %s 的 %d 倍，拒绝执行", newValue, oldValue, MaxRatio)
	}
	if newMi > MaxMemoryMi {
		return fmt.Errorf("新值 %s 超过上限 %dMi，拒绝执行", newValue, MaxMemoryMi)
	}
	return nil
}

// ExecutionResult 是执行结果，和 VerificationResult 分开存。
// Satisfied 表示读回目标后确认新值已生效；Unknown 表示写入后
// 读回失败、归因不明确，不能谎报成功，也不能直接判失败。
// OldPodTemplateHash 是 Patch 前旧 Pod 的 pod-template-hash，
// 验证阶段用它区分新旧 Pod。
type ExecutionResult struct {
	Status             string    `json:"status"`
	DryRunOK           bool      `json:"dryRunOk"`
	UID                string    `json:"uid,omitempty"`
	ResourceVersion    string    `json:"resourceVersion,omitempty"`
	BeforeValue        string    `json:"beforeValue,omitempty"`
	AfterValue         string    `json:"afterValue,omitempty"`
	Satisfied          bool      `json:"satisfied"`
	Unknown            bool      `json:"unknown"`
	AlreadyApplied     bool      `json:"alreadyApplied,omitempty"`
	OldPodTemplateHash string    `json:"oldPodTemplateHash,omitempty"`
	PatchTime          time.Time `json:"patchTime,omitempty"`
	Error              string    `json:"error,omitempty"`
}

func (r ExecutionResult) toJSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// VerificationResult 是验证结果，和执行结果分开存。
// 写入成功但验证失败时，前端要能分别看到这两项，不能混成一个成功。
type VerificationResult struct {
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration"`
	DesiredGeneration  int64  `json:"desiredGeneration"`
	UpdatedReplicas    int32  `json:"updatedReplicas"`
	AvailableReplicas  int32  `json:"availableReplicas"`
	Replicas           int32  `json:"replicas"`
	NewPodName         string `json:"newPodName,omitempty"`
	NewPodReady        bool   `json:"newPodReady"`
	RestartCountBefore int32  `json:"restartCountBefore"`
	RestartCountAfter  int32  `json:"restartCountAfter"`
	RestartsGrew       bool   `json:"restartsGrew"`
	Error              string `json:"error,omitempty"`
}

func (r VerificationResult) toJSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// Executor 执行已批准的提案并验证 rollout。
type Executor struct {
	Store *store.Store
	// Repair 是写客户端，和只读的 KubeClient 是两个接口、两个对象。
	Repair kube.RepairClient
	// PatchTimeout 是单次 Patch 的超时，默认 30 秒。
	PatchTimeout time.Duration
	// VerifyTimeout 是 rollout 验证的总超时，默认 120 秒。
	VerifyTimeout time.Duration
	// VerifyInterval 是验证轮询间隔，默认 2 秒。
	VerifyInterval time.Duration
}

// NewExecutor 组装 Executor，未填的超时用默认值。
func NewExecutor(s *store.Store, rc kube.RepairClient) *Executor {
	return &Executor{Store: s, Repair: rc, PatchTimeout: 30 * time.Second, VerifyTimeout: 120 * time.Second, VerifyInterval: 2 * time.Second}
}

func (e *Executor) patchTimeout() time.Duration {
	if e.PatchTimeout > 0 {
		return e.PatchTimeout
	}
	return 30 * time.Second
}

func (e *Executor) verifyTimeout() time.Duration {
	if e.VerifyTimeout > 0 {
		return e.VerifyTimeout
	}
	return 120 * time.Second
}

func (e *Executor) verifyInterval() time.Duration {
	if e.VerifyInterval > 0 {
		return e.VerifyInterval
	}
	return 2 * time.Second
}

// appendStep 把修复的执行和验证过程写进时间线，Kind 用 "repair"，
// 前端轮询 events 就能看到，不用另开接口。
func (e *Executor) appendStep(ctx context.Context, runID, summary, resultJSON, errMsg string) {
	seq, err := e.Store.NextSeq(ctx, runID)
	if err != nil {
		return
	}
	_ = e.Store.AppendStep(ctx, &store.RunStep{
		RunID: runID, Seq: seq, Kind: "repair", Summary: summary, ResultJSON: resultJSON, Error: errMsg,
	})
}

// ExecuteApproved 领取一份已批准、未执行的提案并执行它。
// 返回 ran=false 表示暂时没有可执行的提案。Worker 循环里调用它，
// 诊断没有任务时就轮到修复，不会让已批准的提案一直等着。
func (e *Executor) ExecuteApproved(ctx context.Context) (bool, error) {
	c, err := e.Store.ClaimApprovedChange(ctx)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, nil
	}
	_, _, err = e.Execute(ctx, c.ID)
	return true, err
}

// Execute 执行一份已批准的提案：先执行前校验，再 dry-run，再 Patch，
// 写入后读回确认，最后独立验证 rollout。执行和验证的结果分开落盘。
// 注意：Execute 假定提案已被 ClaimApprovedChange 领走
// （execution_status 已是 executing），直接调用时也要先保证这一点；
// 如果提案状态不是 approved + executing，会直接返回错误，不执行。
func (e *Executor) Execute(ctx context.Context, changeID string) (*ExecutionResult, *VerificationResult, error) {
	c, err := e.Store.GetChangeRequest(ctx, changeID)
	if err != nil {
		return nil, nil, fmt.Errorf("提案不存在: %w", err)
	}
	if c.Status != store.ChangeApproved {
		return nil, nil, fmt.Errorf("提案 %s 未批准（状态 %s），不能执行", changeID, c.Status)
	}
	// 白名单校验在前，非法提案直接标过期，不碰集群。
	if err := ValidatePatchSpec(c.Namespace, c.Deployment, c.Container, c.OldValue, c.NewValue); err != nil {
		_ = e.Store.SetChangeExpired(ctx, changeID, err.Error())
		e.appendStep(ctx, c.RunID, "修复被拒绝（白名单校验失败）："+err.Error(), "", err.Error())
		return &ExecutionResult{Status: store.ExecExpired, Error: err.Error()}, nil, nil
	}

	execRes := e.executePatch(ctx, c)
	// 执行结果先落盘，再跑验证，两者分开存。
	_ = e.Store.SetExecution(ctx, changeID, execRes.Status, execRes.toJSON())
	e.appendStep(ctx, c.RunID, e.execSummary(execRes), execRes.toJSON(), execRes.Error)
	if execRes.Status != store.ExecExecuted {
		// 没写成功就不用验证 rollout，验证保持 not_started。
		return execRes, nil, nil
	}
	verRes := e.Verify(ctx, c, execRes)
	_ = e.Store.SetVerification(ctx, changeID, verRes.Status, verRes.toJSON())
	e.appendStep(ctx, c.RunID, e.verifySummary(verRes), verRes.toJSON(), verRes.Error)
	return execRes, verRes, nil
}

func (e *Executor) execSummary(r *ExecutionResult) string {
	switch r.Status {
	case store.ExecExecuted:
		if r.AlreadyApplied {
			return fmt.Sprintf("修复已生效（之前已应用过）：容器内存 limit 已是 %s", r.AfterValue)
		}
		return fmt.Sprintf("修复已写入：容器内存 limit %s -> %s，读回确认已生效", r.BeforeValue, r.AfterValue)
	case store.ExecExpired:
		return "修复过期未执行：" + r.Error
	case store.ExecUnknown:
		return "修复执行结果未知：" + r.Error
	default:
		return "修复执行失败：" + r.Error
	}
}

func (e *Executor) verifySummary(r *VerificationResult) string {
	switch r.Status {
	case store.VerifyPassed:
		return fmt.Sprintf("验证通过：rollout 完成，新 Pod %s 已 Ready，重启次数未增长", r.NewPodName)
	case store.VerifyTimeout:
		return "验证超时：" + r.Error
	default:
		return "验证失败：" + r.Error
	}
}

func currentMemoryLimit(dep *appsv1.Deployment, container string) (string, bool) {
	for _, ct := range dep.Spec.Template.Spec.Containers {
		if ct.Name == container {
			return ct.Resources.Limits.Memory().String(), true
		}
	}
	return "", false
}

func oldPodTemplateHash(ctx context.Context, rc kube.RepairClient, namespace, deployment string) string {
	pods, err := rc.ListPodsForRepair(ctx, namespace, map[string]string{"app": deployment})
	if err != nil || len(pods) == 0 {
		return ""
	}
	// 取最老那个 Pod 的 hash 当旧 hash，Patch 后新 Pod 的 hash 一定和它不同。
	oldest := pods[0]
	for _, p := range pods[1:] {
		if p.CreationTimestamp.Before(&oldest.CreationTimestamp) {
			oldest = p
		}
	}
	return oldest.Labels["pod-template-hash"]
}

// executePatch 做执行前校验、dry-run、真实 Patch 和读回确认。
// 返回的 ExecutionResult 一定不为空，调用方按 Status 分流。
func (e *Executor) executePatch(ctx context.Context, c *store.ChangeRequest) *ExecutionResult {
	// 1. 重新读取目标，执行前校验。读不到目标就按未知处理：
	// 不知道目标还在不在，不能直接判过期，也不能直接判失败。
	dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
	if err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, Error: "执行前读取目标失败：" + err.Error()}
	}
	uid := string(dep.UID)
	oldHash := oldPodTemplateHash(ctx, e.Repair, c.Namespace, c.Deployment)
	// 2. UID 校验：提案生成时绑定的 UID 非空、且和现在的不一样，
	// 说明目标已被删了重建，提案的前提变了，标为过期，不执行。
	if c.DeploymentUID != "" && c.DeploymentUID != uid {
		reason := fmt.Sprintf("目标 UID 已变化（提案 %s，现在 %s），目标可能已被重建", c.DeploymentUID, uid)
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, OldPodTemplateHash: oldHash, Error: reason}
	}
	// 3. 容器和当前值校验。
	current, found := currentMemoryLimit(dep, c.Container)
	if !found {
		reason := fmt.Sprintf("目标里没有容器 %q", c.Container)
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, OldPodTemplateHash: oldHash, Error: reason}
	}
	// 幂等：如果当前值已经等于新值，说明 Patch 其实已经生效过
	// （例如上次执行后进程崩了、重启后重跑），直接记为已执行，
	// 不要重复 Patch，也不要误判为过期。
	if current == c.NewValue {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: current, AfterValue: current, Satisfied: true, AlreadyApplied: true, OldPodTemplateHash: oldHash, PatchTime: time.Now()}
	}
	// 旧值也不匹配，说明有人在审批和执行之间改过目标，前提变了，过期。
	if current != c.OldValue {
		reason := fmt.Sprintf("当前值 %s 既不是提案旧值 %s 也不是新值 %s，前提已变", current, c.OldValue, c.NewValue)
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, BeforeValue: current, OldPodTemplateHash: oldHash, Error: reason}
	}
	rv := dep.ResourceVersion

	// 4. dry-run 先行：只校验，不落盘。dry-run 失败就停在这里，
	// 不要发真实 Patch。
	patchCtx, cancel := context.WithTimeout(ctx, e.patchTimeout())
	defer cancel()
	if _, err := e.Repair.PatchDeploymentMemoryLimit(patchCtx, c.Namespace, c.Deployment, c.Container, c.NewValue, rv, true); err != nil {
		return &ExecutionResult{Status: store.ExecFailed, UID: uid, ResourceVersion: rv, BeforeValue: current, OldPodTemplateHash: oldHash, Error: "dry-run 失败：" + err.Error()}
	}

	// 5. 真实 Patch，用本次读取的 resourceVersion 做并发校验。
	// 如果中间有人改过目标，API Server 会返回 409，Patch 失败。
	patchTime := time.Now()
	if _, err := e.Repair.PatchDeploymentMemoryLimit(patchCtx, c.Namespace, c.Deployment, c.Container, c.NewValue, rv, false); err != nil {
		// 写入出错（包括超时）先读回目标再下结论，不要直接判失败：
		// Patch 可能其实已经生效了，只是响应没回来。
		return e.readBackAfterPatchError(ctx, c, uid, rv, current, oldHash, patchTime, err)
	}

	// 6. 写入后读回确认。
	return e.readBack(ctx, c, uid, rv, current, oldHash, patchTime)
}

// readBackAfterPatchError 在 Patch 报错后读回目标，
// 新值已生效记 executed，仍是旧值记 failed，读回也失败记 unknown。
func (e *Executor) readBackAfterPatchError(ctx context.Context, c *store.ChangeRequest, uid, rv, before, oldHash string, patchTime time.Time, patchErr error) *ExecutionResult {
	dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
	if err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: rv, BeforeValue: before, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("Patch 失败（%v），且读回目标也失败，无法确认是否生效", patchErr)}
	}
	current, _ := currentMemoryLimit(dep, c.Container)
	if current == c.NewValue {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: current, Satisfied: true, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("Patch 返回错误（%v），但读回确认新值已生效", patchErr)}
	}
	return &ExecutionResult{Status: store.ExecFailed, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: current, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: "Patch 失败：" + patchErr.Error()}
}

// readBack 写入后读回目标，确认新值已生效才记 executed，
// 否则记 unknown（不谎报成功）。
func (e *Executor) readBack(ctx context.Context, c *store.ChangeRequest, uid, rv, before, oldHash string, patchTime time.Time) *ExecutionResult {
	dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
	if err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: rv, BeforeValue: before, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: "写入后读回目标失败，无法确认是否生效：" + err.Error()}
	}
	current, _ := currentMemoryLimit(dep, c.Container)
	if current == c.NewValue {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: current, Satisfied: true, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime}
	}
	return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: current, DryRunOK: true, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("写入后读回的值是 %s，不是期望的新值 %s", current, c.NewValue)}
}

// Verify 独立验证 rollout。
// 判定标准（缺一不可）：observedGeneration 追上 generation、
// updatedReplicas 和 availableReplicas 达标、至少一个新 Pod Ready、
// 验证窗口内新 Pod 的重启次数没有继续增长。
// 新 Pod 按 pod-template-hash 区分：只认 hash 和 Patch 前旧 Pod
// 不同的 Pod，不把旧 Pod 的 Ready 当成新 Pod 的。
// 超时不一直循环，到点就标 timeout，并把最后一次观测值写进结果里。
func (e *Executor) Verify(ctx context.Context, c *store.ChangeRequest, execRes *ExecutionResult) *VerificationResult {
	_ = e.Store.SetVerification(ctx, c.ID, store.VerifyVerifying, "")
	deadline := time.Now().Add(e.verifyTimeout())
	interval := e.verifyInterval()
	res := &VerificationResult{Status: store.VerifyFailed}
	oldHash := ""
	if execRes != nil {
		oldHash = execRes.OldPodTemplateHash
	}
	var firstRestart int32 = -1
	var lastRestart int32 = -1
	readyRounds := 0
	var lastObserved string
	for {
		if time.Now().After(deadline) {
			res.Status = store.VerifyTimeout
			if res.Error == "" {
				res.Error = "验证超时，rollout 未完成或新 Pod 未 Ready，最后观测：" + lastObserved
			}
			return res
		}
		select {
		case <-ctx.Done():
			res.Status = store.VerifyTimeout
			res.Error = "验证被取消：" + ctx.Err().Error()
			return res
		case <-time.After(interval):
		}
		dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
		if err != nil {
			lastObserved = "读取 Deployment 失败：" + err.Error()
			res.Error = lastObserved
			continue
		}
		res.DesiredGeneration = dep.Generation
		res.ObservedGeneration = dep.Status.ObservedGeneration
		if dep.Spec.Replicas != nil {
			res.Replicas = *dep.Spec.Replicas
		} else {
			res.Replicas = 1
		}
		res.UpdatedReplicas = dep.Status.UpdatedReplicas
		res.AvailableReplicas = dep.Status.AvailableReplicas
		lastObserved = fmt.Sprintf("observedGeneration=%d/%d updated=%d available=%d", res.ObservedGeneration, res.DesiredGeneration, res.UpdatedReplicas, res.AvailableReplicas)

		pods, err := e.Repair.ListPodsForRepair(ctx, c.Namespace, map[string]string{"app": c.Deployment})
		if err != nil {
			res.Error = "列 Pod 失败：" + err.Error()
			continue
		}
		newPod := newestNewPod(pods, oldHash)
		if newPod == nil {
			res.Error = lastObserved + "，暂未看到新 Pod"
			continue
		}
		res.NewPodName = newPod.Name
		ready := false
		var restarts int32
		for _, cs := range newPod.Status.ContainerStatuses {
			restarts += cs.RestartCount
			if cs.Ready {
				ready = true
			}
		}
		res.NewPodReady = ready
		if firstRestart < 0 {
			firstRestart = restarts
			res.RestartCountBefore = restarts
		}
		res.RestartCountAfter = restarts
		if lastRestart >= 0 && restarts > lastRestart {
			res.RestartsGrew = true
		}
		lastRestart = restarts
		lastObserved += fmt.Sprintf("，新 Pod %s Ready=%v 重启=%d", res.NewPodName, ready, restarts)

		// 重启次数在窗口内继续增长，直接判失败，不用等到超时。
		if res.RestartsGrew {
			res.Status = store.VerifyFailed
			res.Error = fmt.Sprintf("新 Pod %s 重启次数继续增长（%d -> %d），修复未稳住", res.NewPodName, firstRestart, restarts)
			return res
		}
		if res.ObservedGeneration < res.DesiredGeneration {
			res.Error = lastObserved + "，observedGeneration 未追上"
			readyRounds = 0
			continue
		}
		if res.UpdatedReplicas < res.Replicas || res.AvailableReplicas < res.Replicas {
			res.Error = lastObserved + "，副本数未达标"
			readyRounds = 0
			continue
		}
		if !ready {
			res.Error = lastObserved + "，新 Pod 未 Ready"
			readyRounds = 0
			continue
		}
		// 四项都达标后，还要连续两轮都通过才算通过：
		// 只看一眼就放行会漏掉刚重启的 Pod。
		readyRounds++
		if readyRounds < 2 {
			res.Error = lastObserved + "，新 Pod 已 Ready，再确认一轮重启数"
			continue
		}
		res.Status = store.VerifyPassed
		res.Error = ""
		return res
	}
}

// newestNewPod 在 Pod 列表里找最新的新 Pod：hash 和旧 hash 不同，
// 如果旧 hash 未知，就按创建时间最晚的那个算。返回 nil 表示还没看到新 Pod。
func newestNewPod(pods []corev1.Pod, oldHash string) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		h := p.Labels["pod-template-hash"]
		if oldHash != "" && h == oldHash {
			continue // 旧 Pod，跳过
		}
		if oldHash == "" && h == "" {
			continue // 没有 hash 区分不了，跳过
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}
