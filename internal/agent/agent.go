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
	"strconv"
	"strings"
	"time"

	"kubecause/internal/store"
)

// NodeTools 是 MCP 节点工具的调用口（见 internal/mcpclient）：
// Agent 不关心背后是 MCP 子进程还是测试替身，只按名字调。
// 没有 MCP Client 时为 nil，节点工具不可用，校验层也会拒绝调它们。
type NodeTools interface {
	HasTool(name string) bool
	CallTool(ctx context.Context, name string, args map[string]string) (ToolOutput, error)
}

// nodeToolNames 是已注册的 MCP 节点工具名：
// 启动时 main.go 从 MCP Client 的 tools/list 结果注册进来，
// 校验和分发只认注册过的名字，模型编一个没注册的名字会被拒绝。
var nodeToolNames = map[string]bool{}

// RegisterNodeTools 注册 MCP 节点工具名（main.go 在 MCP Client 接通后调一次）。
func RegisterNodeTools(names []string) {
	for _, n := range names {
		nodeToolNames[n] = true
	}
}

// IsNodeTool 判断一个工具名是不是已注册的 MCP 节点工具。
func IsNodeTool(name string) bool {
	return nodeToolNames[name]
}

// nodeNameRe 是 Kubernetes 节点名的规则（DNS 子域名，小写字母、数字、-、.），
// 和 MCP Server 侧（internal/node.ValidateNodeName）同一条规则：
// 模型给的节点名先在这里拦一道，带 shell 元字符的到不了 MCP Server。
var nodeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// validateNodeToolCall 校验一次节点工具调用：
// 节点工具不走 namespace 那套（它们查的是节点，不是某个 namespace 下的资源），
// 只校验 node 参数（必填、格式合法）和 node_logs 的 lines（1-500 的数字）。
// 值本身再由 MCP Server 侧校验一次，这里是第一道关。
func validateNodeToolCall(call *ToolCall) error {
	if call.Name == "node_list" {
		return nil
	}
	node := call.Args["node"]
	if node == "" {
		return fmt.Errorf("工具 %s 缺少 node 参数", call.Name)
	}
	if len(node) > 253 || !nodeNameRe.MatchString(node) {
		return fmt.Errorf("节点名格式非法: %q", node)
	}
	if call.Name == "node_logs" {
		if s := call.Args["lines"]; s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 500 {
				return fmt.Errorf("lines 参数 %q 非法：应为 1-500 的数字", s)
			}
		}
	}
	return nil
}

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
	"list_namespaces":      true,
	"list_deployments":     true,
	"list_problem_pods":    true,
	"list_pods":            true,
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

// ModelStep 是模型一次决策的结果：要么调一批工具，要么交最终报告。
// 一次返回里的多个工具调用要全部执行（按返回顺序），不能只取第一个、
// 其余静默丢掉——模型以为它们都执行了。
type ModelStep struct {
	IsFinal   bool
	ToolCalls []*ToolCall
	Report    *Report
}

// ModelInput 是每次问模型时给它的上下文。
type ModelInput struct {
	Namespace string
	Kind      string
	Name      string
	Question  string
	StepIndex int // 已经走了几轮，模型据此决定下一步，不会无限循环
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
	// 节点工具走自己的校验分支：不查 namespace（它们查节点），
	// 只校验 node 参数；没注册过的节点工具名在这里就被拒绝，
	// 不会进后面的 k8s 工具白名单。
	if IsNodeTool(call.Name) {
		return validateNodeToolCall(call)
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
	// MCP 是 MCP 节点工具的调用口（nil 时节点工具不可用）：
	// main.go 在 MCP Client 接通后才填，模型调 node_* 时走这里，
	// 不经过 ToolsAdapter——节点工具不在 Tools 接口里。
	// 没有 MCP Client 的部署（fake 模式、MCP_NODE_TOOLS=0、
	// 或 mcp-server 拉不起来）这个字段就是 nil，诊断照常跑。
	MCP NodeTools
	// OnStep 在每一步工具调用完成后调用，参数是待持久化的步骤
	// （Seq 由调用方在写库时确定）。如果 OnStep 返回错误，循环停止。
	OnStep func(ctx context.Context, step *store.RunStep) error
}

// Run 执行一次诊断循环。
//   - priorSteps 是重启后已提交的步骤，Agent 会复用它们的证据；
//     剩余预算 = MaxSteps - 已用轮次（issue #6：按模型轮次算，
//     不按 tool_result 行数算——一轮里模型可以返回多个工具调用，
//     按行数算会多扣预算）。
//   - 轮次上限和时限是防失控的两道闸：模型一旦开始瞎循环，
//     到 8 轮或 60 秒就强制收敛，返回已有证据下的报告，
//     而不是一直烧 token、一直调工具。
func (a *Agent) Run(ctx context.Context, run *store.Run, priorSteps []store.RunStep) (*Report, error) {
	// 时限用 context 超时实现，到点后工具调用会被取消。
	ctx, cancel := context.WithTimeout(ctx, time.Duration(run.TimeoutSeconds)*time.Second)
	defer cancel()

	evidence := map[string]bool{}
	evidenceSummary := map[string]string{}
	// 预算按模型轮次算（issue #6）：恢复时从已提交步骤的 Round 列
	// 取最大轮次 +1。一轮里模型可以返回多个工具调用，
	// 按 tool_result 行数算会多扣预算（两轮 5 个调用，恢复后只剩 3 轮而不是 6 轮）。
	// Round = -1 是“未知/老数据”哨兵：迁移前没有 Round 列的步骤读出来
	// 是 -1，和明确跑过第 0 轮的步骤（Round=0）区分开。
	// 只有全是 -1（真正老数据）时才按 0 算、宽容地不扣预算。
	roundsUsed := 0
	maxRound := -1
	for _, st := range priorSteps {
		if st.Kind == "tool_result" && st.Round > maxRound {
			maxRound = st.Round
		}
	}
	if maxRound >= 0 {
		roundsUsed = maxRound + 1
	}
	// 证据只登记成功步骤（issue #8）：失败、校验拒绝的步骤可以留在
	// 历史里供模型理解错误过程，但不能作为可引用的成功证据。
	// 证据 ID 在执行成功时分配并写进步骤（EvidenceID），恢复时直接读，
	// 不再从步骤序号推算 ev-<Seq>——校验失败的步骤占 Seq 但不消耗 ev 号，
	// 推算出来的 ID 在恢复前后会对不上（成功步骤运行时是 ev-1，恢复后变 ev-2）。
	// 老数据没有 EvidenceID：只贡献 CallID，新分配的 ev 号从 1 起。
	nextEvidenceSeq := 1
	for _, st := range priorSteps {
		if st.Kind != "tool_result" || st.Error != "" {
			continue
		}
		if st.CallID != "" {
			evidence[st.CallID] = true
			evidenceSummary[st.CallID] = st.Summary
		}
		if st.EvidenceID != "" {
			evidence[st.EvidenceID] = true
			evidenceSummary[st.EvidenceID] = st.Summary
			if n, ok := parseEvidenceSeq(st.EvidenceID); ok && n >= nextEvidenceSeq {
				nextEvidenceSeq = n + 1
			}
		}
	}

	remaining := run.MaxSteps - roundsUsed
	if remaining < 0 {
		remaining = 0
	}

	// 每轮工具调用上限（issue #6）：一轮里最多处理 cap 个不同的调用，
	// 超出的不执行，记一步错误告诉模型分轮调。
	// 0 表示用默认值 5，不是不限。
	maxToolCallsPerRound := run.MaxToolCallsPerRound
	if maxToolCallsPerRound <= 0 {
		maxToolCallsPerRound = 5
	}

	var lastReport *Report
	for i := 0; i < remaining; i++ {
		if err := ctx.Err(); err != nil {
			return a.finishWithPartial(run, evidenceSummary, lastReport, "诊断超时，已用已有证据收敛"), nil
		}
		// 本轮的轮次号：恢复后已用的轮次数 + 本次循环的第几轮。
		// 每一步（包括校验失败、执行失败的错误步）都带上它，
		// 恢复时才能从 Round 列算出已用轮次。
		roundIndex := roundsUsed + i
		in := ModelInput{
			Namespace: run.Namespace,
			Kind:      run.Kind,
			Name:      run.Name,
			Question:  run.Question,
			StepIndex: roundIndex,
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
		if len(step.ToolCalls) == 0 {
			return nil, fmt.Errorf("模型既没有调工具也没有交报告")
		}
		// 一次返回里的多个工具调用按返回顺序全部执行：
		// 以前这里只取第一个、其余静默丢掉，模型却以为它们都执行了。
		// 校验、执行、记步都按调用逐个走；某一个失败只跳过它自己，
		// 不影响同批其他调用。步数预算按模型轮次算（外层 for i 循环），
		// 一轮里调多个工具不额外占预算。
		//
		// 同批内去重（issue #7）：同一轮里同一个调用出现两次，
		// 第二次不重新执行，把第一次的结果作为它的 tool 结果回传。
		// 只记成功的调用：失败的调用不进表，同一批里重复出现就再执行一次。
		// 跨轮、跨恢复不再去重：所有工具都是只读查询，重查无害；
		// 模型看得到历史结果，只有它认为需要 fresh 数据时才会重查——
		// 模型决定、代码不拦。恢复后“相同调用实例不重复执行”靠历史本身保证：
		// 已执行的调用结果已在 priorSteps 里，模型不会无故重发；
		// 如果它主动重发，那就是它要 fresh 数据，按合法重查处理。
		type batchResult struct {
			resultJSON string
			summary    string
		}
		batchResults := map[string]*batchResult{}
		// processed 是本轮已处理的不同调用数：回放不占名额，
		// 校验失败的占一个名额——它已经进了一次处理流程。
		processed := 0
		for _, call := range step.ToolCalls {
			fingerprint := call.Name + "|" + mustJSON(call.Args)
			if prev, ok := batchResults[fingerprint]; ok {
				// 同批重复：不重新执行。给这个 call ID 单独记一步，
				// 结果和第一次的一样，模型能按 call ID 对上。
				// 不另分一个 ev 号：这是同一份证据，凭各自的 call ID 引用。
				if a.OnStep != nil {
					if err := a.OnStep(ctx, &store.RunStep{
						RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
						CallID: call.ID, ArgsJSON: mustJSON(call.Args),
						ResultJSON: prev.resultJSON, Summary: prev.summary,
						Round: roundIndex,
					}); err != nil {
						return nil, err
					}
				}
				evidence[call.ID] = true
				evidenceSummary[call.ID] = prev.summary
				priorSteps = append(priorSteps, store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					ResultJSON: prev.resultJSON, Summary: prev.summary,
					Round: roundIndex,
				})
				continue
			}
			// 每轮工具调用上限（issue #6）：一批里超过上限的调用不执行，
			// 记一步错误告诉模型分轮调——模型下一轮能看到并重试，
			// 而不是以为它们都执行了。
			if processed >= maxToolCallsPerRound {
				overLimit := fmt.Sprintf("本轮工具调用已达上限（%d 个），未执行，请分轮调用", maxToolCallsPerRound)
				errStep := &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					Summary: overLimit, Error: overLimit, Round: roundIndex,
				}
				if a.OnStep != nil {
					if err := a.OnStep(ctx, errStep); err != nil {
						return nil, err
					}
				}
				priorSteps = append(priorSteps, *errStep)
				continue
			}
			processed++
			// 先校验，再执行，顺序不能反。
			if err := ValidateToolCall(run.Namespace, call); err != nil {
				// 校验失败记一步错误，不执行，跳过这个调用、继续同批下一个。
				// 关键修复：错误步也要追加进 priorSteps，否则模型下一次
				// 看到的历史还是空的，会重复调同一个非法调用，把 8 步烧光。
				// 之前真模型在 kind 上就是这样连调 8 次 list_workload_pods 的。
				errStep := &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					Summary: "工具调用被拒绝：" + err.Error(), Error: err.Error(),
					Round: roundIndex,
				}
				if a.OnStep != nil {
					if err := a.OnStep(ctx, errStep); err != nil {
						return nil, err
					}
				}
				priorSteps = append(priorSteps, *errStep)
				continue
			}
			start := time.Now()
			out, execErr := a.executeTool(ctx, call)
			duration := time.Since(start)
			if execErr != nil {
				// 工具失败也要把错误步追加进 priorSteps，让模型下一次能看到
				// “这个工具刚才失败了，原因是什么”，从而换个工具重试，
				// 而不是原地重复调同一个失败的工具。
				errStep := &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					Summary: "工具执行失败：" + execErr.Error(), Error: execErr.Error(),
					DurationMs: duration.Milliseconds(), Round: roundIndex,
				}
				if a.OnStep != nil {
					if err := a.OnStep(ctx, errStep); err != nil {
						return nil, err
					}
				}
				priorSteps = append(priorSteps, *errStep)
				// 工具失败不让整个任务崩掉，跳过这个调用、继续同批下一个，
				// 模型可以换工具重试。
				continue
			}
			// 证据 ID 只在成功时分配（issue #8）：失败的执行不消耗 ev 号，
			// 否则恢复时按成功步骤重算会和运行时对不上。
			// 证据 ID 同时记两份：call.ID（模型看到的）和 ev-<n>（报告引用的），
			// 报告校验时两边都认，避免模型引用格式不一致导致证据被误过滤。
			evidenceID := fmt.Sprintf("ev-%d", nextEvidenceSeq)
			nextEvidenceSeq++
			evidence[evidenceID] = true
			evidence[call.ID] = true
			evidenceSummary[evidenceID] = out.Summary
			evidenceSummary[call.ID] = out.Summary
			resultJSON, _ := json.Marshal(out.Data)
			batchResults[fingerprint] = &batchResult{resultJSON: string(resultJSON), summary: out.Summary}
			if a.OnStep != nil {
				if err := a.OnStep(ctx, &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					ResultJSON: string(resultJSON), Summary: out.Summary,
					DurationMs: duration.Milliseconds(), Round: roundIndex,
					EvidenceID: evidenceID,
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
				ResultJSON: string(resultJSON), Round: roundIndex, EvidenceID: evidenceID,
			})
		}
		lastReport = nil
	}

	// 轮数耗尽还没交报告：用已有证据收敛出一份“证据不足”的报告，
	// 而不是报错丢掉已经查到的东西。
	return a.finishWithPartial(run, evidenceSummary, lastReport, "已达最大轮数，用已有证据收敛"), nil
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
// 节点工具先走 MCP：MCP 为 nil 或名字不在 MCP 工具表里时，
// 说明这次调用本来就不是节点工具，再走下面的 k8s 分发。
func (a *Agent) executeTool(ctx context.Context, call *ToolCall) (ToolOutput, error) {
	if a.MCP != nil && a.MCP.HasTool(call.Name) {
		return a.MCP.CallTool(ctx, call.Name, call.Args)
	}
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

// parseEvidenceSeq 从 "ev-<n>" 里解析 n，格式不对返回 ok=false。
// 恢复时用它从已持久化的 EvidenceID 算下一个 ev 号。
func parseEvidenceSeq(id string) (int, bool) {
	if !strings.HasPrefix(id, "ev-") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "ev-"))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
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
