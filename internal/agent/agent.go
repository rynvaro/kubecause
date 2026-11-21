// Package agent 是诊断循环的核心：模型选工具，后端校验并执行，
// 再把工具结果喂回模型，直到模型给出最终诊断或耗尽预算。
//
// 为什么要把校验放在模型外面：模型输出是不可信输入，它可能编一个
// 不存在的工具、写错 namespace、或者跨 namespace 查别的任务的目标。
// 所有工具调用都必须先过 ValidateToolCall 这一关，
// 校验失败直接记一步错误，不执行，循环继续，不会让模型拿到不该拿的数据。
// 注意：查系统命名空间不算“不该拿”——读是只读，kube-system 也能查，
// 只是写操作被 repair 层挡住。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"kubecause/internal/store"
)

// 允许的工具名，白名单，模型只能叫这些，多一个都不行。
// 前 6 个是单次诊断的定向工具，后 5 个是发现工具：
// 对话模式里模型不知道目标在哪时，先用它们看全貌、找问题，
// 再用定向工具深挖。发现工具全部是只读的。
var allowedTools = map[string]bool{
	"get_pod":              true,
	"list_workload_pods":   true,
	"get_events":           true,
	"get_pod_logs":         true,
	"get_deployment":       true,
	"search_runbooks":      true,
	"list_namespaces":      true,
	"list_deployments":     true,
	"list_problem_pods":    true,
	"list_pods":            true,
	"get_cluster_overview": true,
}

// 发现工具集合：namespace 为空表示查全部，不用和任务 namespace 一致。
var discoveryTools = map[string]bool{
	"list_namespaces":   true,
	"list_deployments":  true,
	"list_problem_pods": true,
	"list_pods":         true,
	"get_cluster_overview": true,
}

// 日志上限：一次最多 100 行、64KB。Pod 日志可能很大，不截断会把
// 模型上下文撑爆，也会把 SQLite 里的步骤撑大。截断后要在摘要里写清楚。
const (
	MaxLogLines = 100
	MaxLogBytes = 64 * 1024
)

// PatchOperation 是通用 Patch 里的一条操作（Slice 9）。
// 执行器不再按故障类型分分支：OOM、ImagePullBackOff、主动换镜像版本，
// 都走同一套 op/path/value。Op 目前只支持 "replace"，
// 不做 add/remove，避免模型误删资源里的字段。
// Path 例如：/spec/template/spec/containers/0/image（改镜像）、
// /spec/template/spec/containers/0/resources/limits/memory（改内存 limit）、
// /spec/replicas（改副本数）。下标是容器在 Deployment 里的顺序，
// Worker 生成提案时会按容器名重算下标，模型给错下标也不会改错容器。
// 本类型是 store.PatchOperation 的别名：store 不能 import agent
// （agent 已经 import store），所以规范定义放在 store，
// agent、repair、worker 共用同一份结构，JSON 形状完全一致。
type PatchOperation = store.PatchOperation

// SuggestedRepair 是模型在诊断报告里可选提出的修复建议。
// 它只是一个建议，不是可直接执行的 Patch：Worker 收到后还要经过
// repair.ValidatePatch 白名单校验、读集群现状核对旧值、人工审批，
// 三关都过了才生成提案。Patches 是通用 Patch 列表，
// 不再只认内存——镜像、内存、副本数都用同一套结构表达，
// 故障类型变了也不用给执行器加分支。
// Field / OldValue / NewValue 是 Slice 8 及以前的旧字段，保留做兼容：
// 老的 memory-only 建议卡（field=memoryLimit）读进来时，
// Normalize() 会把它转成一条内存 limit 的 Patch，
// 旧测试和 eval 不用改；新模型应直接给 Patches，
// 两个都给了时以 Patches 为准。
type SuggestedRepair struct {
	Namespace  string           `json:"namespace"`
	Deployment string           `json:"deployment"`
	Container  string           `json:"container"`
	Patches    []PatchOperation `json:"patches,omitempty"`
	Field      string           `json:"field,omitempty"` // 旧字段，已废弃，见 Normalize
	OldValue   string           `json:"oldValue,omitempty"`
	NewValue   string           `json:"newValue,omitempty"`
	Reason     string           `json:"reason"`
}

// legacyFieldPath 把旧字段名映射到通用 Patch 的 path，
// 容器下标先写 0，Worker 生成提案时按容器名重算。
func legacyFieldPath(field string) (string, bool) {
	switch field {
	case "memoryLimit", "":
		return "/spec/template/spec/containers/0/resources/limits/memory", true
	case "memoryRequest":
		return "/spec/template/spec/containers/0/resources/requests/memory", true
	case "cpuLimit":
		return "/spec/template/spec/containers/0/resources/limits/cpu", true
	case "cpuRequest":
		return "/spec/template/spec/containers/0/resources/requests/cpu", true
	case "image":
		return "/spec/template/spec/containers/0/image", true
	case "replicas":
		return "/spec/replicas", true
	default:
		return "", false
	}
}

// ToPatches 返回这张建议卡的通用 Patch 列表。
// 有 Patches 就直接用；只有旧字段时按 Field 映射成一条 Patch，
// 这样 Slice 8 的 memory-only 卡不用改代码也能走通用执行器。
// 既没有 Patches 也没有可用的旧字段时返回 nil，表示给不出结构化建议。
func (sr *SuggestedRepair) ToPatches() []PatchOperation {
	if sr == nil {
		return nil
	}
	if len(sr.Patches) > 0 {
		out := make([]PatchOperation, len(sr.Patches))
		copy(out, sr.Patches)
		return out
	}
	if sr.NewValue == "" {
		return nil
	}
	path, ok := legacyFieldPath(sr.Field)
	if !ok {
		return nil
	}
	return []PatchOperation{{Op: "replace", Path: path, Value: sr.NewValue}}
}

// Normalize 把旧字段的建议卡补成通用 Patch 格式。
// 有 Patches 的不改；只有旧字段的，Patches 按旧字段填上，
// OldValue / NewValue 留着做展示和执行前提核对用。
// 调用点：ValidateReport（报告进 Worker 之前统一归一化），
// 以及 Stub / synthesizeRepair 构造建议卡之后。
func (sr *SuggestedRepair) Normalize() {
	if sr == nil || len(sr.Patches) > 0 {
		return
	}
	if patches := sr.ToPatches(); len(patches) > 0 {
		sr.Patches = patches
	}
}

// PatchValueString 把 Patch 的 Value 转成可展示的字符串，
// Value 在 JSON 里可能是字符串、数字或布尔，
// 直接 fmt 打印数字会带小数点，这里按类型处理。
func PatchValueString(v any) string {
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
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// SuggestedCreation 是模型在诊断报告里可选提出的创建建议（Slice 10）。
// 它只是一个建议，不是可直接执行的创建：Worker 收到后还要经过
// repair.ValidateCreation 校验、核对目标不存在、人工审批，
// 三关都过了才生成创建提案。创建只针对 Deployment，
// 不直接创建裸 Pod——用户说“创建一个 Pod”时也按 Deployment 给，
// 并在对话里说明原因。类型是 store.CreationSpec 的别名，
// 和 PatchOperation 一样，规范定义放在 store，避免循环引用。
type SuggestedCreation = store.CreationSpec

// Report 是诊断报告，字段和架构文档一致。
type Report struct {
	Facts           []string `json:"facts"`
	Cause           string   `json:"cause"`
	EvidenceIDs     []string `json:"evidenceIds"`
	Recommendations []string `json:"recommendations"`
	OpenQuestions   []string `json:"openQuestions"`
	Confidence      string   `json:"confidence"` // high / medium / low
	// SuggestedRepair 是可选的修复建议，模型觉得有把握才给，
	// 没把握时留空，Worker 不会凭空生成提案。
	SuggestedRepair *SuggestedRepair `json:"suggestedRepair,omitempty"`
	// SuggestedCreation 是可选的创建建议（Slice 10），
	// 只有用户明确要求创建 Deployment 时才给，
	// 没把握或信息不全时留空，Worker 不会凭空生成创建提案。
	SuggestedCreation *SuggestedCreation `json:"suggestedCreation,omitempty"`
}

// dedupeStrings 按顺序去重：同一条事实、建议、待确认、证据 ID
// 只保留第一次出现。Slice 10 修的 bug：正式诊断里同一条事实
// 出现了两遍（“Deployment demo-api, container demo-api memory limit 256Mi”
// 重复），原因是多步工具摘要拼出来的事实本身就有重复，
// 报告归一化时不去重就原样落盘、原样展示。
// 去重只在归一化这一层做，不改工具摘要本身，
// 工具时间线仍能看到每一步的原始记录。
func dedupeStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// Normalize 把 Report 里的切片字段补成空数组而不是 nil。
// 为什么必须做：真模型返回的 JSON 缺字段时，Go 解出来是 nil，
// 再序列化成 JSON 就是 null，前端读 report.facts.length 会直接抛错，
// 整个 React 应用白屏（Slice 7 用户在 Mac 真机上遇到的就是这个）。
// 归一化后序列化出来是 []，前端读 length 得到 0，不会崩。
// 调用点：ValidateReport（单次诊断和对话的报告都要过它），
// 以及 model.parseDiagnosisArgs（模型刚解析完报告时就归一化）。
func (r *Report) Normalize() {
	if r == nil {
		return
	}
	if r.Facts == nil {
		r.Facts = []string{}
	}
	if r.EvidenceIDs == nil {
		r.EvidenceIDs = []string{}
	}
	if r.Recommendations == nil {
		r.Recommendations = []string{}
	}
	if r.OpenQuestions == nil {
		r.OpenQuestions = []string{}
	}
	// 同一条事实/建议/待确认/证据只保留一次（见 dedupeStrings）。
	r.Facts = dedupeStrings(r.Facts)
	r.EvidenceIDs = dedupeStrings(r.EvidenceIDs)
	r.Recommendations = dedupeStrings(r.Recommendations)
	r.OpenQuestions = dedupeStrings(r.OpenQuestions)
	if r.SuggestedRepair != nil {
		r.SuggestedRepair.Normalize()
	}
}

// ToolCall 是模型返回的一次结构化工具调用。
type ToolCall struct {
	ID   string            `json:"id"`
	Name string            `json:"name"`
	Args map[string]string `json:"args"`
}

// ToolOutput 是工具执行后返回给模型的内容。
// Summary 是给模型和前端看的一句话，Data 是结构化证据。
type ToolOutput struct {
	Summary string `json:"summary"`
	Data    any    `json:"data"`
}

// Tools 是 Agent 能调的工具集合，由 internal/run 的适配器实现，
// 底层再分发到 kube 客户端和知识库。这样 Agent 本身不依赖
// 具体的 K8s 实现，fake 和 kind 可以互换，测试也能直接换桩。
type Tools interface {
	GetPod(ctx context.Context, namespace, name string) (ToolOutput, error)
	ListWorkloadPods(ctx context.Context, namespace, deployment string) (ToolOutput, error)
	GetEvents(ctx context.Context, namespace, name string) (ToolOutput, error)
	GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool) (ToolOutput, error)
	GetDeployment(ctx context.Context, namespace, name string) (ToolOutput, error)
	SearchRunbooks(ctx context.Context, query string) (ToolOutput, error)
	// 发现工具（只读）：namespace 为空表示查全部命名空间。
	ListNamespaces(ctx context.Context) (ToolOutput, error)
	ListDeployments(ctx context.Context, namespace string) (ToolOutput, error)
	ListProblemPods(ctx context.Context, namespace string) (ToolOutput, error)
	ListPods(ctx context.Context, namespace string) (ToolOutput, error)
	GetClusterOverview(ctx context.Context) (ToolOutput, error)
}

// ModelStep 是模型一次决策的结果：要么调一个工具，要么交最终报告。
type ModelStep struct {
	IsFinal  bool
	ToolCall *ToolCall
	Report   *Report
}

// ModelInput 是每次问模型时给它的上下文。
type ModelInput struct {
	Namespace string
	Kind      string
	Name      string
	Question  string
	StepIndex int // 已经走了几步，模型据此决定下一步，不会无限循环
	History   []store.RunStep
	Evidence  map[string]string // evidenceID -> 摘要
}

// Model 是模型的接口，桩模型和真模型都实现它。
// Slice 1 只有桩模型，真模型在 Slice 2 接入。
type Model interface {
	Next(ctx context.Context, in ModelInput) (ModelStep, error)
}

var (
	nsRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// ValidateToolCall 校验一次工具调用。非法调用直接返回错误，不执行。
// 规则：
//   - 工具名必须在白名单里；
//   - namespace 必须和任务一致，禁止跨 namespace 查；
//   - 资源名不能为空，不能含 / 和 ..，防止路径穿越式的滥用。
//
// 注意：读系统命名空间是允许的——get/list 都是只读，不改集群，
// 查 kube-system 做诊断是合法需求，行里会打系统标记。
// 写操作仍然被挡住，那是 repair.ValidatePatchSpec 的职责。
func ValidateToolCall(namespace string, call *ToolCall) error {
	if call == nil {
		return fmt.Errorf("工具调用为空")
	}
	if !allowedTools[call.Name] {
		return fmt.Errorf("未知工具: %s", call.Name)
	}
	if call.Name == "search_runbooks" {
		if strings.TrimSpace(call.Args["query"]) == "" {
			return fmt.Errorf("search_runbooks 需要 query 参数")
		}
		return nil
	}
	// 发现工具：namespace 为空表示查全部，合法；
	// 指定了 namespace 时必须和任务一致（单次诊断的范围约束）。
	// 系统命名空间不再拒绝：读是只读，查 kube-system 合法。
	if discoveryTools[call.Name] {
		ns := call.Args["namespace"]
		if ns == "" {
			return nil
		}
		if !nsRe.MatchString(ns) {
			return fmt.Errorf("namespace 格式非法: %q", ns)
		}
		if namespace != "" && ns != namespace {
			return fmt.Errorf("目标 namespace %q 与任务 %q 不一致，拒绝执行", ns, namespace)
		}
		return nil
	}
	ns := call.Args["namespace"]
	if ns == "" {
		return fmt.Errorf("缺少 namespace 参数")
	}
	if ns != namespace {
		return fmt.Errorf("目标 namespace %q 与任务 %q 不一致，拒绝执行", ns, namespace)
	}
	if !nsRe.MatchString(ns) {
		return fmt.Errorf("namespace 格式非法: %q", ns)
	}
	// 目标名：get_pod / get_pod_logs 用 pod，list_workload_pods 和
	// get_deployment 用 deployment，get_events 用 name。
	target := firstNonEmpty(call.Args["pod"], call.Args["deployment"], call.Args["name"])
	if target == "" {
		return fmt.Errorf("工具 %s 缺少目标资源名", call.Name)
	}
	if strings.Contains(target, "/") || strings.Contains(target, "..") {
		return fmt.Errorf("目标资源名非法: %q", target)
	}
	if !nameRe.MatchString(target) {
		return fmt.Errorf("目标资源名格式非法: %q", target)
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ValidateReport 校验诊断报告引用的证据 ID 是否都属于当前任务。
// 模型可能凭空编一个证据 ID，或者引用别的任务的证据，
// 这里直接过滤掉，不让它进报告。证据不足时不报错，
// 而是把 Confidence 降级，让报告诚实地说“还没查够”。
func ValidateReport(rep *Report, validEvidence map[string]bool) {
	if rep == nil {
		return
	}
	// 先归一化：缺失的切片字段补成空数组，序列化后是 [] 而不是 null，
	// 前端读 .length 不会抛错（见 Report.Normalize 的注释）。
	rep.Normalize()
	kept := make([]string, 0, len(rep.EvidenceIDs))
	for _, id := range rep.EvidenceIDs {
		if validEvidence[id] {
			kept = append(kept, id)
		}
	}
	rep.EvidenceIDs = kept
	if len(kept) == 0 && len(rep.Facts) == 0 {
		rep.Confidence = "low"
		if len(rep.OpenQuestions) == 0 {
			rep.OpenQuestions = []string{"证据不足，需要补查 Pod 状态、事件和日志"}
		}
	}
	if rep.Confidence == "" {
		rep.Confidence = "medium"
	}
	// 修复建议归一化成通用 Patch（Slice 9）：有 Patches 的直接保留，
	// 只有旧字段（field=oldValue->newValue）的按字段映射成 Patch，
	// 字段映射不到（模型编了一个不存在的字段）时才清掉。
	// 校验字段值本身是 repair.ValidatePatch 的职责（白名单、格式、倍数），
	// 这里只做形状归一化，不替校验层做决定。
	if sr := rep.SuggestedRepair; sr != nil {
		sr.Normalize()
		if len(sr.Patches) == 0 {
			rep.SuggestedRepair = nil
		}
	}
	// 创建建议只做形状检查（Slice 10）：namespace / 名字 / 镜像
	// 三个缺一不可，缺了说明模型没给全，不能生成创建提案。
	// 值本身的合法性（镜像格式、副本数、资源上限）是
	// repair.ValidateCreation 的职责，这里不替校验层做决定。
	if sc := rep.SuggestedCreation; sc != nil {
		if strings.TrimSpace(sc.Namespace) == "" || strings.TrimSpace(sc.Name) == "" || strings.TrimSpace(sc.Image) == "" {
			rep.SuggestedCreation = nil
		}
	}
}

// Agent 持有工具、模型和持久化回调。
// OnStep 由 Worker 传入，负责把每一步写进 SQLite；
// Agent 本身不直接碰数据库，方便单测。
type Agent struct {
	Tools Tools
	Model Model
	// OnStep 在每一步工具调用完成后调用，参数是待持久化的步骤
	// （Seq 由调用方在写库时确定）。如果 OnStep 返回错误，循环停止。
	OnStep func(ctx context.Context, step *store.RunStep) error
}

// Run 执行一次诊断循环。
//   - priorSteps 是重启后已提交的步骤，Agent 会复用它们的证据，
//     不再重复调已经调过的工具；剩余预算 = MaxSteps - 已用工具步数。
//   - 步数上限和时限是防失控的两道闸：模型一旦开始瞎循环，
//     到 8 步或 60 秒就强制收敛，返回已有证据下的报告，
//     而不是一直烧 token、一直调工具。
func (a *Agent) Run(ctx context.Context, run *store.Run, priorSteps []store.RunStep) (*Report, error) {
	// 时限用 context 超时实现，到点后工具调用会被取消。
	ctx, cancel := context.WithTimeout(ctx, time.Duration(run.TimeoutSeconds)*time.Second)
	defer cancel()

	evidence := map[string]bool{}
	evidenceSummary := map[string]string{}
	usedToolFingerprints := map[string]bool{}
	toolStepsUsed := 0
	for _, st := range priorSteps {
		if st.Kind == "tool_result" && st.CallID != "" {
			evidence[st.CallID] = true
			evidenceSummary[st.CallID] = st.Summary
			// 用工具名+参数做指纹，重启后同一个调用不再重复执行。
			usedToolFingerprints[st.ToolName+"|"+st.ArgsJSON] = true
			toolStepsUsed++
		}
		if st.Kind == "tool_result" {
			// 兼容旧步骤：CallID 即证据 ID。
			if st.ResultJSON != "" {
				evidence[fmt.Sprintf("ev-%d", st.Seq)] = true
			}
		}
	}

	// 证据 ID 按步骤序号生成，ev-<seq>，保证属于当前任务、可追溯。
	nextEvidenceSeq := 1
	for _, st := range priorSteps {
		if st.Seq >= nextEvidenceSeq {
			nextEvidenceSeq = st.Seq + 1
		}
	}

	remaining := run.MaxSteps - toolStepsUsed
	if remaining < 0 {
		remaining = 0
	}

	var lastReport *Report
	for i := 0; i < remaining; i++ {
		if err := ctx.Err(); err != nil {
			return a.finishWithPartial(run, evidenceSummary, lastReport, "诊断超时，已用已有证据收敛"), nil
		}
		in := ModelInput{
			Namespace: run.Namespace,
			Kind:      run.Kind,
			Name:      run.Name,
			Question:  run.Question,
			StepIndex: toolStepsUsed + i,
			History:   priorSteps,
			Evidence:  evidenceSummary,
		}
		step, err := a.Model.Next(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("模型决策失败: %w", err)
		}
		if step.IsFinal {
			if step.Report == nil {
				return nil, fmt.Errorf("模型提交了空报告")
			}
			ValidateReport(step.Report, evidence)
			return step.Report, nil
		}
		if step.ToolCall == nil {
			return nil, fmt.Errorf("模型既没有调工具也没有交报告")
		}
		call := step.ToolCall
		// 先校验，再执行，顺序不能反。
		if err := ValidateToolCall(run.Namespace, call); err != nil {
			// 校验失败记一步错误，不执行，继续让模型换个合法调用。
			// 关键修复：错误步也要追加进 priorSteps，否则模型下一次
			// 看到的历史还是空的，会重复调同一个非法调用，把 8 步烧光。
			// 之前真模型在 kind 上就是这样连调 8 次 list_workload_pods 的。
			errStep := &store.RunStep{
				RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
				CallID: call.ID, ArgsJSON: mustJSON(call.Args),
				Summary: "工具调用被拒绝：" + err.Error(), Error: err.Error(),
			}
			if a.OnStep != nil {
				_ = a.OnStep(ctx, errStep)
			}
			priorSteps = append(priorSteps, *errStep)
			continue
		}
		// 重启恢复：指纹命中说明这一步已经提交过，直接跳过不重复调。
		fingerprint := call.Name + "|" + mustJSON(call.Args)
		if usedToolFingerprints[fingerprint] {
			continue
		}
		start := time.Now()
		out, execErr := a.executeTool(ctx, call)
		duration := time.Since(start)
		evidenceID := fmt.Sprintf("ev-%d", nextEvidenceSeq)
		nextEvidenceSeq++
		if execErr != nil {
			// 工具失败也要把错误步追加进 priorSteps，让模型下一次能看到
			// “这个工具刚才失败了，原因是什么”，从而换个工具重试，
			// 而不是原地重复调同一个失败的工具。
			errStep := &store.RunStep{
				RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
				CallID: call.ID, ArgsJSON: mustJSON(call.Args),
				Summary: "工具执行失败：" + execErr.Error(), Error: execErr.Error(),
				DurationMs: duration.Milliseconds(),
			}
			if a.OnStep != nil {
				_ = a.OnStep(ctx, errStep)
			}
			priorSteps = append(priorSteps, *errStep)
			// 工具失败不让整个任务崩掉，继续循环，模型可以换工具重试。
			continue
		}
		// 证据 ID 同时记两份：call.ID（模型看到的）和 ev-<seq>（报告引用的），
		// 报告校验时两边都认，避免模型引用格式不一致导致证据被误过滤。
		evidence[evidenceID] = true
		evidence[call.ID] = true
		evidenceSummary[evidenceID] = out.Summary
		evidenceSummary[call.ID] = out.Summary
		usedToolFingerprints[fingerprint] = true
		resultJSON, _ := json.Marshal(out.Data)
		if a.OnStep != nil {
			if err := a.OnStep(ctx, &store.RunStep{
				RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
				CallID: call.ID, ArgsJSON: mustJSON(call.Args),
				ResultJSON: string(resultJSON), Summary: out.Summary,
				DurationMs: duration.Milliseconds(),
			}); err != nil {
				return nil, err
			}
		}
		// 把这一步的结果追加进 priorSteps，让桩模型能按顺序推进。
		// Slice 2 补上 ResultJSON：Stub 和真模型都靠历史里的结果
		// 判断场景（例如 ImagePullBackOff 只在 Events / 日志的
		// ResultJSON 里出现，Summary 里不一定有），不带它
		// imagepull / probe 用例会识别不到信号。
		priorSteps = append(priorSteps, store.RunStep{
			RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
			CallID: call.ID, ArgsJSON: mustJSON(call.Args), Summary: out.Summary,
			ResultJSON: string(resultJSON),
		})
		lastReport = nil
	}

	// 步数耗尽还没交报告：用已有证据收敛出一份“证据不足”的报告，
	// 而不是报错丢掉已经查到的东西。
	return a.finishWithPartial(run, evidenceSummary, lastReport, "已达最大步数，用已有证据收敛"), nil
}

func (a *Agent) finishWithPartial(run *store.Run, evidenceSummary map[string]string, _ *Report, note string) *Report {
	facts := []string{note}
	ids := make([]string, 0, len(evidenceSummary))
	for id, summary := range evidenceSummary {
		facts = append(facts, summary)
		ids = append(ids, id)
	}
	return &Report{
		Facts:           facts,
		Cause:           "证据不足，无法确定根因",
		EvidenceIDs:     ids,
		Recommendations: []string{"补查 Pod 状态、事件和日志后再下结论"},
		OpenQuestions:   []string{"需要更多证据确认根因"},
		Confidence:      "low",
	}
}

// executeTool 按工具名分发，参数已经过 ValidateToolCall 校验。
func (a *Agent) executeTool(ctx context.Context, call *ToolCall) (ToolOutput, error) {
	ns := call.Args["namespace"]
	switch call.Name {
	case "get_pod":
		return a.Tools.GetPod(ctx, ns, call.Args["pod"])
	case "list_workload_pods":
		return a.Tools.ListWorkloadPods(ctx, ns, call.Args["deployment"])
	case "get_events":
		return a.Tools.GetEvents(ctx, ns, call.Args["name"])
	case "get_pod_logs":
		previous := call.Args["previous"] == "true"
		return a.Tools.GetPodLogs(ctx, ns, call.Args["pod"], call.Args["container"], previous)
	case "get_deployment":
		return a.Tools.GetDeployment(ctx, ns, call.Args["deployment"])
	case "search_runbooks":
		return a.Tools.SearchRunbooks(ctx, call.Args["query"])
	case "list_namespaces":
		return a.Tools.ListNamespaces(ctx)
	case "list_deployments":
		return a.Tools.ListDeployments(ctx, call.Args["namespace"])
	case "list_problem_pods":
		return a.Tools.ListProblemPods(ctx, call.Args["namespace"])
	case "list_pods":
		return a.Tools.ListPods(ctx, call.Args["namespace"])
	case "get_cluster_overview":
		return a.Tools.GetClusterOverview(ctx)
	default:
		return ToolOutput{}, fmt.Errorf("未知工具: %s", call.Name)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TruncateLog 按行数和字节数截断日志，返回截断后的内容和是否被截断。
func TruncateLog(log string) (string, bool) {
	lines := strings.Split(log, "\n")
	truncated := false
	if len(lines) > MaxLogLines {
		lines = lines[:MaxLogLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > MaxLogBytes {
		out = out[:MaxLogBytes]
		truncated = true
	}
	return out, truncated
}
