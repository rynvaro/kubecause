// Package repair 负责审批后的修复：条件 Patch 与 rollout 验证。
//
// Slice 9 起执行器是通用的，不再按故障类型分分支：
// 任意 namespace / Deployment / 容器都可以提修复建议，
// 但只能改白名单里的字段（镜像、内存 limit/request、CPU limit/request、
// 副本数），值必须合法，内存新值还要满足“大于旧值、不超过旧值 4 倍、
// 不超过 4Gi”。OOM、ImagePullBackOff、主动换镜像版本，
// 都走同一套 Patch 流程：模型决策改什么，后端只校验能不能改，
// 校验过了、人工批了，才执行。安全边界分三层，缺一不可：
//  1. 白名单：只允许改白名单里的 path，值必须合法，见 ValidatePatch；
//  2. 执行前校验：重新读取目标，核对 UID、容器名、变更前旧值，
//     再用本次读取的 resourceVersion 做条件更新，防并发覆盖；
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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"kubecause/internal/kube"
	"kubecause/internal/store"
)

// Proposal 是修复提案，和 change_requests 表一一对应。
// 一份提案是一组通用 Patch：指定 Deployment 的若干个字段改成新值。
// 字段和值都要由修复模块校验，不允许模型直接拼一个任意 Patch。
// OldValue / NewValue 只保留做展示用，取第一个 Patch 的改前/改后值；
// 执行时以 Patches 为准。老提案没有 Patches 时，
// CreateProposal 会从 OldValue / NewValue 反推一条内存 Patch。
//
// Slice 10 起提案分两种：Kind="patch"（默认，改已有 Deployment 的字段），
// Kind="create"（创建提案，Creation 里是完整规格，Patches 为空）。
// 两种提案走同一套审批 / 执行 / 验证流程：建议卡 → 创建可审批任务 →
// 人工审批 → Worker 执行 → 验证 rollout，不为创建单独写一套流程，
// 以后改成 manager agent 时创建能力可以直接复用。
type Proposal struct {
	RunID         string
	Namespace     string
	Deployment    string
	Container     string
	OldValue      string // 例如 128Mi（展示用，第一个 Patch 的旧值）
	NewValue      string // 例如 256Mi（展示用，第一个 Patch 的新值）
	Patches       []store.PatchOperation
	DeploymentUID string // 提案生成时绑定的目标 UID，空表示老提案未绑定
	// Creation 是创建提案的规格（Slice 10），只有 Kind="create" 时才填：
	// 填了它就表示这是一份创建提案，执行时走 CreateDeployment 而不是 Patch。
	Creation *store.CreationSpec
}

// CreateProposal 生成一份待审批提案，写入 change_requests 表。
// 提案一旦生成就不可变：改目标、改前后值都要生成新提案，
// 不能原地改旧提案，这样审批时看到的东西和执行时用的东西才一致。
func CreateProposal(s *store.Store, p *Proposal) (*store.ChangeRequest, error) {
	// 创建提案（Slice 10）：规格先补默认值、再过 ValidateCreation，
	// 校验不过不生成提案。创建没有旧值，NewValue 存镜像做展示用。
	if p.Creation != nil {
		spec := NormalizeCreation(p.Creation)
		if err := ValidateCreation(spec); err != nil {
			return nil, err
		}
		return s.CreateChangeRequest(context.Background(), &store.ChangeRequest{
			RunID: p.RunID, Namespace: spec.Namespace, Deployment: spec.Name,
			Container: spec.Container, OldValue: "", NewValue: spec.Image,
			Kind: "create", Creation: spec,
		})
	}
	patches := p.Patches
	if len(patches) == 0 {
		// 老调用方只给了 old/new 值：按旧字段反推一条内存 limit 的 Patch，
		// 让老提案也走通用执行器，不用再维护两套创建逻辑。
		if p.OldValue == "" || p.NewValue == "" {
			return nil, fmt.Errorf("提案缺少 Patch，也没有新旧值")
		}
		if err := ValidatePatchSpec(p.Namespace, p.Deployment, p.Container, p.OldValue, p.NewValue); err != nil {
			return nil, err
		}
		patches = []store.PatchOperation{{
			Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: p.NewValue,
		}}
	} else {
		if err := ValidatePatch(p.Namespace, p.Deployment, patches); err != nil {
			return nil, err
		}
	}
	return s.CreateChangeRequest(context.Background(), &store.ChangeRequest{
		RunID: p.RunID, Namespace: p.Namespace, Deployment: p.Deployment,
		Container: p.Container, OldValue: p.OldValue, NewValue: p.NewValue,
		Patches: patches, DeploymentUID: p.DeploymentUID,
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

var (
	imageRe = regexp.MustCompile(`^[a-z0-9._/-]+(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
	// 容器下标路径，例如 /spec/template/spec/containers/0/image。
	containerPathRe = regexp.MustCompile(`^/spec/template/spec/containers/(\d+)/(image|resources/(limits|requests)/(memory|cpu))$`)
)

// parsePatchPath 解析一条 Patch 的 path，返回它是哪一类字段、
// 容器下标（副本数时为 -1）。白名单外的 path 在这里就被挡掉，
// 不会进到执行器。
func parsePatchPath(path string) (kind string, containerIdx int, err error) {
	if path == "/spec/replicas" {
		return "replicas", -1, nil
	}
	m := containerPathRe.FindStringSubmatch(path)
	if m == nil {
		return "", -1, fmt.Errorf("path 不在白名单里: %q", path)
	}
	idx := 0
	for _, c := range m[1] {
		idx = idx*10 + int(c-'0')
	}
	switch m[2] {
	case "image":
		return "image", idx, nil
	case "resources/limits/memory":
		return "memoryLimit", idx, nil
	case "resources/requests/memory":
		return "memoryRequest", idx, nil
	case "resources/limits/cpu":
		return "cpuLimit", idx, nil
	case "resources/requests/cpu":
		return "cpuRequest", idx, nil
	}
	return "", -1, fmt.Errorf("path 不在白名单里: %q", path)
}

// isBlacklistedPath 对明确禁止的字段给一句清楚的拒绝原因，
// 而不是只说“不在白名单里”：这些字段改了可能让 Pod 起不来
// （command/args、hostNetwork、privileged），或改了资源身份
// （metadata/name、namespace），先禁掉，以后用户明确要求时再放开。
func isBlacklistedPath(path string) string {
	for _, bad := range []string{
		"hostNetwork", "hostPID", "privileged",
		"/metadata/namespace", "/metadata/name",
		"/command", "/args",
	} {
		if strings.Contains(path, bad) {
			return bad
		}
	}
	return ""
}

// patchNumber 把 Patch 的 Value 转成数字（副本数用），
// JSON 里数字可能是 float64，模型也可能给成 "3" 这样的字符串。
func patchNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// validatePatchValue 只校验值的格式，不看集群现状：
// 镜像名要合法、内存要 Mi/Gi 结尾、CPU 要合法数量、副本数 1-10。
// 和现状有关的检查（内存新值相对旧值的倍数、容器下标越界）
// 在 ValidatePatchWithCurrent 里做，那里才有现状可读。
func validatePatchValue(kind string, value any) error {
	switch kind {
	case "image":
		s, ok := value.(string)
		if !ok || s == "" || strings.Contains(s, " ") || !imageRe.MatchString(s) {
			return fmt.Errorf("镜像名非法: %v", value)
		}
	case "memoryLimit", "memoryRequest":
		s, ok := value.(string)
		if !ok || !isMemoryValue(s) {
			return fmt.Errorf("内存值必须是 Mi 或 Gi 结尾，例如 128Mi、2Gi，得到 %v", value)
		}
	case "cpuLimit", "cpuRequest":
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("CPU 值必须是字符串，例如 500m、1，得到 %v", value)
		}
		q, err := resource.ParseQuantity(s)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("CPU 值非法: %q", s)
		}
	case "replicas":
		n, ok := patchNumber(value)
		if !ok || n != float64(int64(n)) || n < 1 || n > 10 {
			return fmt.Errorf("副本数必须是 1-10 之间的整数，得到 %v", value)
		}
	}
	return nil
}

// ValidatePatch 是通用 Patch 的白名单校验，第一道防线。
// 只按字段分，不按故障分：OOM、ImagePullBackOff、主动变更
// 都过同一套检查，新增一种故障不用给执行器加分支。
// 规则：
//   - 只允许改 Deployment（调用方保证目标是 Deployment，
//     这里再校验 namespace / Deployment 名格式）；
//   - op 只支持 "replace"，不做 add/remove，避免删字段；
//   - path 必须在白名单里：副本数、容器镜像、容器内存/CPU 的
//     limit/request；黑名单字段（hostNetwork、privileged、
//     command/args、metadata/name 等）明确拒绝；
//   - 每个 Patch 的值格式要合法，不合法就拒绝整个建议，
//     不给一张半合法的卡；
//   - 系统命名空间一律拒绝。
//
// 和集群现状有关的检查（容器下标越界、内存倍数）不在这里，
// 在 ValidatePatchWithCurrent 里，调用方读到现状后再调它。
func ValidatePatch(namespace, deployment string, patches []store.PatchOperation) error {
	if namespace == "" || deployment == "" {
		return fmt.Errorf("提案缺少目标信息")
	}
	if namespace == "kube-system" || namespace == "kube-public" || namespace == "kube-node-lease" {
		return fmt.Errorf("禁止修改系统命名空间 %q", namespace)
	}
	if !repairNsRe.MatchString(namespace) {
		return fmt.Errorf("namespace 格式非法: %q", namespace)
	}
	if !repairNameRe.MatchString(deployment) {
		return fmt.Errorf("目标名格式非法: %q", deployment)
	}
	if len(patches) == 0 {
		return fmt.Errorf("Patch 列表为空")
	}
	for i, p := range patches {
		if p.Op != "replace" {
			return fmt.Errorf("第 %d 个 Patch 的 op 只支持 replace，得到 %q", i+1, p.Op)
		}
		if bad := isBlacklistedPath(p.Path); bad != "" {
			return fmt.Errorf("禁止修改字段 %q（path %q）", bad, p.Path)
		}
		kind, _, err := parsePatchPath(p.Path)
		if err != nil {
			return fmt.Errorf("第 %d 个 Patch：%w", i+1, err)
		}
		if err := validatePatchValue(kind, p.Value); err != nil {
			return fmt.Errorf("第 %d 个 Patch（%s）：%w", i+1, p.Path, err)
		}
	}
	return nil
}

// checkMemoryBounds 检查内存新值相对旧值的两个上限：
// 新值必须大于旧值、不超过旧值的 MaxRatio 倍、不超过 MaxMemoryMi。
func checkMemoryBounds(oldValue, newValue string) error {
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

// ValidatePatchWithCurrent 在读到集群现状后做第二层校验：
// 容器下标不能越界；内存 limit/request 的新值要满足倍数和上限。
// 内存 request 的旧值可能为空（没配 request），
// 此时只检查新值不超过上限，不做倍数比较。
func ValidatePatchWithCurrent(dep *appsv1.Deployment, patches []store.PatchOperation) error {
	if dep == nil {
		return fmt.Errorf("目标 Deployment 为空")
	}
	containers := dep.Spec.Template.Spec.Containers
	for i, p := range patches {
		kind, idx, err := parsePatchPath(p.Path)
		if err != nil {
			return fmt.Errorf("第 %d 个 Patch：%w", i+1, err)
		}
		if idx >= 0 {
			if idx >= len(containers) {
				return fmt.Errorf("第 %d 个 Patch 的容器下标 %d 越界，目标只有 %d 个容器", i+1, idx, len(containers))
			}
			ct := containers[idx]
			newStr := patchValueString(p.Value)
			switch kind {
			case "memoryLimit":
				if err := checkMemoryBounds(ct.Resources.Limits.Memory().String(), newStr); err != nil {
					return fmt.Errorf("第 %d 个 Patch（%s）：%w", i+1, p.Path, err)
				}
			case "memoryRequest":
				old := ct.Resources.Requests.Memory().String()
				if old != "" && old != "0" {
					if err := checkMemoryBounds(old, newStr); err != nil {
						return fmt.Errorf("第 %d 个 Patch（%s）：%w", i+1, p.Path, err)
					}
				} else if newMi, err := parseMemoryMi(newStr); err != nil || newMi <= 0 || newMi > MaxMemoryMi {
					return fmt.Errorf("第 %d 个 Patch（%s）：新值 %s 超过上限 %dMi", i+1, p.Path, newStr, MaxMemoryMi)
				}
			}
		}
	}
	return nil
}

// patchValueString 把 Patch 的 Value 转成字符串，
// 和 agent.PatchValueString 同样的规则（数字不带小数点），
// repair 包内直接用，避免循环引用。
func patchValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// ResolveContainerIndex 按容器名重算 Patch 里的容器下标。
// 模型给的 path 里下标可能写错（例如多容器 Deployment 里写了 0，
// 实际要改的是第 2 个容器）：这里按建议卡里的容器名找到正确下标，
// 把 /containers/N/ 里的 N 换成正确的，保证改的是模型指明的容器。
// 容器名为空或在 Deployment 里找不到时，原样返回，不猜。
func ResolveContainerIndex(patches []store.PatchOperation, dep *appsv1.Deployment, container string) []store.PatchOperation {
	if container == "" || dep == nil {
		return patches
	}
	idx := -1
	for i, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == container {
			idx = i
			break
		}
	}
	if idx < 0 {
		return patches
	}
	out := make([]store.PatchOperation, len(patches))
	for i, p := range patches {
		out[i] = p
		if m := containerPathRe.FindStringSubmatch(p.Path); m != nil {
			out[i].Path = strings.Replace(p.Path, "/containers/"+m[1]+"/", fmt.Sprintf("/containers/%d/", idx), 1)
		}
	}
	return out
}

// ValidatePatchSpec 是老接口（Slice 4-8 用），保留做兼容：
// 只认内存 limit 的 old->new，内部转成一条通用 Patch 再校验，
// 校验逻辑和新接口同一套，不再维护两份白名单。
// 旧值和集群现状是否一致不在这里查，那是执行前校验（executePatch）
// 重新读目标时核对的，两层分开，职责才清楚。
func ValidatePatchSpec(namespace, deployment, container, oldValue, newValue string) error {
	if namespace == "" || deployment == "" || container == "" {
		return fmt.Errorf("提案缺少目标信息")
	}
	if oldValue == "" || newValue == "" || oldValue == newValue {
		return fmt.Errorf("提案的新旧值非法")
	}
	if !isMemoryValue(oldValue) || !isMemoryValue(newValue) {
		return fmt.Errorf("内存值必须是 Mi 或 Gi 结尾，例如 128Mi、2Gi")
	}
	patches := []store.PatchOperation{{
		Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: newValue,
	}}
	if err := ValidatePatch(namespace, deployment, patches); err != nil {
		return err
	}
	return checkMemoryBounds(oldValue, newValue)
}

// ExecutionResult 是执行结果，和 VerificationResult 分开存。
// Satisfied 表示读回目标后确认新值已生效；Unknown 表示写入后
// 读回失败、归因不明确，不能谎报成功，也不能直接判失败。
// OldPodTemplateHash 是 Patch 前旧 Pod 的 pod-template-hash，
// 验证阶段用它区分新旧 Pod。
type ExecutionResult struct {
	Status             string                 `json:"status"`
	DryRunOK           bool                   `json:"dryRunOk"`
	UID                string                 `json:"uid,omitempty"`
	ResourceVersion    string                 `json:"resourceVersion,omitempty"`
	BeforeValue        string                 `json:"beforeValue,omitempty"`
	AfterValue         string                 `json:"afterValue,omitempty"`
	Patches            []store.PatchOperation `json:"patches,omitempty"`
	Satisfied          bool                   `json:"satisfied"`
	Unknown            bool                   `json:"unknown"`
	AlreadyApplied     bool                   `json:"alreadyApplied,omitempty"`
	OldPodTemplateHash string                 `json:"oldPodTemplateHash,omitempty"`
	PatchTime          time.Time              `json:"patchTime,omitempty"`
	Error              string                 `json:"error,omitempty"`
	// Kind 是执行类型（Slice 10）："patch" 或 "create"，和提案的 Kind 一致，
	// 前端和时间线摘要据此区分“改了什么”和“创建了什么”。
	Kind string `json:"kind,omitempty"`
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
	// 创建提案（Slice 10）：和 Patch 提案走同一条审批后的执行线，
	// 只是执行动作从 Patch 换成 CreateDeployment，验证还是同一套 rollout 验证。
	// 校验不通过的创建提案直接标过期，不碰集群。
	if c.Kind == "create" {
		if c.Creation == nil {
			reason := "创建提案缺少创建规格"
			_ = e.Store.SetChangeExpired(ctx, changeID, reason)
			e.appendStep(ctx, c.RunID, "创建被拒绝（缺少创建规格）："+reason, "", reason)
			return &ExecutionResult{Status: store.ExecExpired, Kind: "create", Error: reason}, nil, nil
		}
		if err := ValidateCreation(c.Creation); err != nil {
			_ = e.Store.SetChangeExpired(ctx, changeID, err.Error())
			e.appendStep(ctx, c.RunID, "创建被拒绝（校验失败）："+err.Error(), "", err.Error())
			return &ExecutionResult{Status: store.ExecExpired, Kind: "create", Error: err.Error()}, nil, nil
		}
		execRes := e.executeCreate(ctx, c)
		return e.finishExecution(ctx, c, execRes)
	}
	// 兜底：Patches 为空的老提案（直接写库、没走 CreateProposal 的），
	// 从 old/new 值反推一条内存 Patch，和读库时的反推保持一致。
	if len(c.Patches) == 0 && c.OldValue != "" && c.NewValue != "" {
		c.Patches = []store.PatchOperation{{
			Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: c.NewValue,
		}}
	}
	// 白名单校验在前，非法提案直接标过期，不碰集群。
	// 静态校验（字段、格式）不过就直接拒绝；现状相关的校验
	// （容器下标、内存倍数）在 executePatch 读到现状后再做。
	if err := ValidatePatch(c.Namespace, c.Deployment, c.Patches); err != nil {
		_ = e.Store.SetChangeExpired(ctx, changeID, err.Error())
		e.appendStep(ctx, c.RunID, "修复被拒绝（白名单校验失败）："+err.Error(), "", err.Error())
		return &ExecutionResult{Status: store.ExecExpired, Error: err.Error()}, nil, nil
	}

	execRes := e.executePatch(ctx, c)
	return e.finishExecution(ctx, c, execRes)
}

// finishExecution 是 Patch 和创建共用的执行收尾：
// 执行结果先落盘，再跑验证，两者分开存。
// 没写成功就不用验证 rollout，验证保持 not_started。
func (e *Executor) finishExecution(ctx context.Context, c *store.ChangeRequest, execRes *ExecutionResult) (*ExecutionResult, *VerificationResult, error) {
	_ = e.Store.SetExecution(ctx, c.ID, execRes.Status, execRes.toJSON())
	e.appendStep(ctx, c.RunID, e.execSummary(c, execRes), execRes.toJSON(), execRes.Error)
	if execRes.Status != store.ExecExecuted {
		// 没写成功就不用验证 rollout，验证保持 not_started。
		return execRes, nil, nil
	}
	verRes := e.Verify(ctx, c, execRes)
	_ = e.Store.SetVerification(ctx, c.ID, verRes.Status, verRes.toJSON())
	e.appendStep(ctx, c.RunID, e.verifySummary(verRes), verRes.toJSON(), verRes.Error)
	return execRes, verRes, nil
}

// patchSummary 把一组 Patch 写成一行摘要：
// "path 旧值 -> 新值；path 旧值 -> 新值"，旧值读不到时只写新值。
func patchSummary(patches []store.PatchOperation, before map[string]string) string {
	parts := make([]string, 0, len(patches))
	for _, p := range patches {
		after := patchValueString(p.Value)
		if old, ok := before[p.Path]; ok && old != "" {
			parts = append(parts, fmt.Sprintf("%s %s -> %s", p.Path, old, after))
		} else {
			parts = append(parts, fmt.Sprintf("%s -> %s", p.Path, after))
		}
	}
	return strings.Join(parts, "；")
}

func (e *Executor) execSummary(c *store.ChangeRequest, r *ExecutionResult) string {
	// 创建提案的摘要（Slice 10）：说清楚创建了什么，而不是按 Patch 的写法说“修复已写入”。
	if c != nil && c.Kind == "create" {
		switch r.Status {
		case store.ExecExecuted:
			if r.AlreadyApplied {
				return fmt.Sprintf("Deployment %s/%s 已存在且配置一致，视为已创建", c.Namespace, c.Deployment)
			}
			return fmt.Sprintf("已创建 Deployment %s/%s（镜像 %s，%d 个副本），读回确认已生效", c.Namespace, c.Deployment, r.AfterValue, replicasOf(c))
		case store.ExecExpired:
			return "创建过期未执行：" + r.Error
		case store.ExecUnknown:
			return "创建执行结果未知：" + r.Error
		default:
			return "创建执行失败：" + r.Error
		}
	}
	switch r.Status {
	case store.ExecExecuted:
		if r.AlreadyApplied {
			return fmt.Sprintf("修复已生效（之前已应用过）：%s", patchSummary(r.Patches, map[string]string{}))
		}
		before := map[string]string{}
		if r.BeforeValue != "" && len(r.Patches) > 0 {
			before[r.Patches[0].Path] = r.BeforeValue
		}
		return fmt.Sprintf("修复已写入：%s，读回确认已生效", patchSummary(r.Patches, before))
	case store.ExecExpired:
		return "修复过期未执行：" + r.Error
	case store.ExecUnknown:
		return "修复执行结果未知：" + r.Error
	default:
		return "修复执行失败：" + r.Error
	}
}

// replicasOf 从创建提案里读副本数，读不到默认 1，只做摘要展示用。
func replicasOf(c *store.ChangeRequest) int {
	if c != nil && c.Creation != nil && c.Creation.Replicas > 0 {
		return c.Creation.Replicas
	}
	return 1
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

// currentValueForPatch 从 Deployment 现状里读一条 Patch 对应的当前值，
// 返回可展示的字符串。内存/CPU 用 resource.Quantity 的规范写法
// （例如 "128Mi"、"500m"），镜像直接比字符串，副本数比数字。
// 找不到容器时返回 found=false。
func currentValueForPatch(dep *appsv1.Deployment, p store.PatchOperation) (string, bool) {
	kind, idx, err := parsePatchPath(p.Path)
	if err != nil {
		return "", false
	}
	if kind == "replicas" {
		if dep.Spec.Replicas == nil {
			return "1", true
		}
		return fmt.Sprintf("%d", *dep.Spec.Replicas), true
	}
	if idx < 0 || idx >= len(dep.Spec.Template.Spec.Containers) {
		return "", false
	}
	ct := dep.Spec.Template.Spec.Containers[idx]
	switch kind {
	case "image":
		return ct.Image, true
	case "memoryLimit":
		return ct.Resources.Limits.Memory().String(), true
	case "memoryRequest":
		return ct.Resources.Requests.Memory().String(), true
	case "cpuLimit":
		return ct.Resources.Limits.Cpu().String(), true
	case "cpuRequest":
		return ct.Resources.Requests.Cpu().String(), true
	}
	return "", false
}

// patchValuesEqual 比较 Patch 的期望值和现状值是否一致。
// 内存/CPU 按数量比较（"256Mi" 和 "256Mi" 相等，"0.5" 和 "500m" 也相等），
// 镜像和副本数按字符串比较（副本数先转成整数再比，避免 "3" 和 "3.0" 不等）。
func patchValuesEqual(kind, current, expected string) bool {
	switch kind {
	case "memoryLimit", "memoryRequest", "cpuLimit", "cpuRequest":
		cq, err1 := resource.ParseQuantity(current)
		eq, err2 := resource.ParseQuantity(expected)
		if err1 != nil || err2 != nil {
			return current == expected
		}
		return cq.Cmp(eq) == 0
	case "replicas":
		cn, ok1 := patchNumber(current)
		en, ok2 := patchNumber(expected)
		if !ok1 || !ok2 {
			return current == expected
		}
		return cn == en
	default:
		return current == expected
	}
}

// currentValues 批量读一组 Patch 的当前值，key 是 path。
func currentValues(dep *appsv1.Deployment, patches []store.PatchOperation) map[string]string {
	out := map[string]string{}
	for _, p := range patches {
		if v, ok := currentValueForPatch(dep, p); ok {
			out[p.Path] = v
		}
	}
	return out
}

// firstPatchBeforeAfter 取第一个 Patch 的改前/改后值，
// 存进 ExecutionResult 的 BeforeValue / AfterValue 做展示和兼容，
// 完整列表在 ExecutionResult.Patches 里。
func firstPatchBeforeAfter(patches []store.PatchOperation, before map[string]string) (string, string) {
	if len(patches) == 0 {
		return "", ""
	}
	return before[patches[0].Path], patchValueString(patches[0].Value)
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
// 通用流程（Slice 9）：不再只拼内存 Patch，
// 而是把模型给的 Patches 列表原样传给 client-go 的 Patch API
// （JSONPatchType），改镜像、改内存、改副本数走同一条路。
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
	// 按容器名重算下标：模型给的 path 里下标可能写错，
	// 这里按提案里的容器名纠正，保证改的是指明的容器。
	patches := ResolveContainerIndex(c.Patches, dep, c.Container)
	// 2. UID 校验：提案生成时绑定的 UID 非空、且和现在的不一样，
	// 说明目标已被删了重建，提案的前提变了，标为过期，不执行。
	if c.DeploymentUID != "" && c.DeploymentUID != uid {
		reason := fmt.Sprintf("目标 UID 已变化（提案 %s，现在 %s），目标可能已被重建", c.DeploymentUID, uid)
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, Patches: patches, OldPodTemplateHash: oldHash, Error: reason}
	}
	// 容器名校验：提案指明的容器必须在目标里，否则前提变了。
	if c.Container != "" {
		found := false
		for _, ct := range dep.Spec.Template.Spec.Containers {
			if ct.Name == c.Container {
				found = true
				break
			}
		}
		if !found {
			reason := fmt.Sprintf("目标里没有容器 %q", c.Container)
			_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
			return &ExecutionResult{Status: store.ExecExpired, UID: uid, Patches: patches, OldPodTemplateHash: oldHash, Error: reason}
		}
	}
	before := currentValues(dep, patches)
	beforeVal, afterVal := firstPatchBeforeAfter(patches, before)
	// 4. 幂等：如果每条 Patch 的当前值都已经等于新值，
	// 说明 Patch 其实已经生效过（例如上次执行后进程崩了、重启后重跑），
	// 直接记为已执行，不要重复 Patch，也不要误判为过期。
	if len(patches) > 0 && e.allPatchesSatisfied(dep, patches) {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: beforeVal, AfterValue: afterVal, Patches: patches, Satisfied: true, AlreadyApplied: true, OldPodTemplateHash: oldHash, PatchTime: time.Now()}
	}
	// 4b. 现状校验：容器下标越界、内存倍数/上限在这里查，
	// 提案生成时校验过一次，执行前还要再查一次现状，
	// 因为审批和执行之间现状可能已经变了。
	// 注意顺序：必须放在幂等判断之后——当前值已经等于新值时，
	// “新值必须大于旧值”这条倍数检查一定不通过（新值等于现状），
	// 但那正是“之前已经执行过”的情况，应该直接记为已执行，
	// 而不是标过期。
	if err := ValidatePatchWithCurrent(dep, patches); err != nil {
		reason := "执行前校验失败：" + err.Error()
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, Patches: patches, OldPodTemplateHash: oldHash, Error: reason}
	}
	// 5. 前提核对：第一个 Patch 的当前值既不是提案旧值也不是新值，
	// 说明有人在审批和执行之间改过目标，前提变了，过期。
	// OldValue 是提案生成时第一个 Patch 的改前值（Worker 按现状填的），
	// 老提案的 OldValue 是内存旧值，同一条逻辑。
	if c.OldValue != "" && len(patches) > 0 {
		kind, _, _ := parsePatchPath(patches[0].Path)
		cur := before[patches[0].Path]
		if !patchValuesEqual(kind, cur, c.OldValue) && !patchValuesEqual(kind, cur, patchValueString(patches[0].Value)) {
			reason := fmt.Sprintf("当前值 %s 既不是提案旧值 %s 也不是新值 %s，前提已变", cur, c.OldValue, patchValueString(patches[0].Value))
			_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
			return &ExecutionResult{Status: store.ExecExpired, UID: uid, BeforeValue: cur, Patches: patches, OldPodTemplateHash: oldHash, Error: reason}
		}
	}
	rv := dep.ResourceVersion
	patchJSON, _ := json.Marshal(patches)

	// 6. dry-run 先行：只校验，不落盘。dry-run 失败就停在这里，
	// 不要发真实 Patch。
	patchCtx, cancel := context.WithTimeout(ctx, e.patchTimeout())
	defer cancel()
	if _, err := e.Repair.PatchDeployment(patchCtx, c.Namespace, c.Deployment, patchJSON, rv, true); err != nil {
		return &ExecutionResult{Status: store.ExecFailed, UID: uid, ResourceVersion: rv, BeforeValue: beforeVal, AfterValue: afterVal, Patches: patches, OldPodTemplateHash: oldHash, Error: "dry-run 失败：" + err.Error()}
	}

	// 7. 条件更新：JSONPatchType 的 patch 体里没有地方放 resourceVersion，
	// 所以在发真实 Patch 之前再读一次目标，核对 resourceVersion 没变，
	// 变了说明中间有人改过目标，停下来标过期，不按旧前提 Patch。
	// 这和原来把 resourceVersion 写进 strategic merge patch 的
	// metadata 是同一层保证，只是换了一种实现方式。
	if dep2, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment); err != nil || dep2.ResourceVersion != rv {
		reason := "执行期间目标被修改（resourceVersion 已变），前提已变"
		if err != nil {
			reason = "真实 Patch 前重读目标失败：" + err.Error()
		}
		_ = e.Store.SetChangeExpired(ctx, c.ID, reason)
		return &ExecutionResult{Status: store.ExecExpired, UID: uid, ResourceVersion: rv, BeforeValue: beforeVal, AfterValue: afterVal, Patches: patches, OldPodTemplateHash: oldHash, Error: reason}
	}

	// 8. 真实 Patch，用同一组 Patches。
	patchTime := time.Now()
	if _, err := e.Repair.PatchDeployment(patchCtx, c.Namespace, c.Deployment, patchJSON, rv, false); err != nil {
		// 写入出错（包括超时）先读回目标再下结论，不要直接判失败：
		// Patch 可能其实已经生效了，只是响应没回来。
		return e.readBackAfterPatchError(ctx, c, patches, uid, rv, beforeVal, oldHash, patchTime, err)
	}

	// 9. 写入后读回确认：逐条核对每条 Patch 的值都已生效。
	return e.readBack(ctx, c, patches, uid, rv, beforeVal, oldHash, patchTime)
}

// readBackAfterPatchError 在 Patch 报错后读回目标，
// 每条 Patch 的新值都已生效记 executed，
// 仍是旧值记 failed，读回也失败记 unknown。
func (e *Executor) readBackAfterPatchError(ctx context.Context, c *store.ChangeRequest, patches []store.PatchOperation, uid, rv, before string, oldHash string, patchTime time.Time, patchErr error) *ExecutionResult {
	dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
	if err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: rv, BeforeValue: before, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("Patch 失败（%v），且读回目标也失败，无法确认是否生效", patchErr)}
	}
	after := currentValues(dep, patches)
	_, afterVal := firstPatchBeforeAfter(patches, after)
	if e.allPatchesSatisfied(dep, patches) {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: afterVal, Satisfied: true, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("Patch 返回错误（%v），但读回确认新值已生效", patchErr)}
	}
	return &ExecutionResult{Status: store.ExecFailed, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: afterVal, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: "Patch 失败：" + patchErr.Error()}
}

// allPatchesSatisfied 检查每条 Patch 的当前值是否都等于期望的新值。
func (e *Executor) allPatchesSatisfied(dep *appsv1.Deployment, patches []store.PatchOperation) bool {
	for _, p := range patches {
		kind, _, err := parsePatchPath(p.Path)
		if err != nil {
			return false
		}
		cur, ok := currentValueForPatch(dep, p)
		if !ok || !patchValuesEqual(kind, cur, patchValueString(p.Value)) {
			return false
		}
	}
	return true
}

// readBack 写入后读回目标，确认每条 Patch 的新值都已生效才记 executed，
// 否则记 unknown（不谎报成功）。
func (e *Executor) readBack(ctx context.Context, c *store.ChangeRequest, patches []store.PatchOperation, uid, rv, before string, oldHash string, patchTime time.Time) *ExecutionResult {
	dep, err := e.Repair.GetDeploymentForRepair(ctx, c.Namespace, c.Deployment)
	if err != nil {
		return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: rv, BeforeValue: before, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: "写入后读回目标失败，无法确认是否生效：" + err.Error()}
	}
	after := currentValues(dep, patches)
	_, afterVal := firstPatchBeforeAfter(patches, after)
	if e.allPatchesSatisfied(dep, patches) {
		return &ExecutionResult{Status: store.ExecExecuted, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: afterVal, Satisfied: true, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime}
	}
	return &ExecutionResult{Status: store.ExecUnknown, UID: uid, ResourceVersion: dep.ResourceVersion, BeforeValue: before, AfterValue: afterVal, DryRunOK: true, Patches: patches, OldPodTemplateHash: oldHash, PatchTime: patchTime, Error: fmt.Sprintf("写入后读回的值是 %v，不是期望的新值", after)}
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
