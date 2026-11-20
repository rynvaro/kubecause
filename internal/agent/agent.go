// Package agent 是诊断循环的核心：模型选工具，后端校验并执行，
// 再把工具结果喂回模型，直到模型给出最终诊断或耗尽预算。
//
// 为什么要把校验放在模型外面：模型输出是不可信输入，它可能编一个
// 不存在的工具、写错 namespace、或者要查 kube-system 这种不该查的
// 地方。所有工具调用都必须先过 ValidateToolCall 这一关，
// 校验失败直接记一步错误，不执行，循环继续，不会让模型拿到不该拿的数据。
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

// 允许的工具名，白名单，模型只能叫这 6 个，多一个都不行。
var allowedTools = map[string]bool{
	"get_pod":           true,
	"list_workload_pods": true,
	"get_events":        true,
	"get_pod_logs":      true,
	"get_deployment":    true,
	"search_runbooks":   true,
}

// 日志上限：一次最多 100 行、64KB。Pod 日志可能很大，不截断会把
// 模型上下文撑爆，也会把 SQLite 里的步骤撑大。截断后要在摘要里写清楚。
const (
	MaxLogLines = 100
	MaxLogBytes = 64 * 1024
)

// Report 是诊断报告，字段和架构文档一致。
type Report struct {
	Facts           []string `json:"facts"`
	Cause           string   `json:"cause"`
	EvidenceIDs     []string `json:"evidenceIds"`
	Recommendations []string `json:"recommendations"`
	OpenQuestions   []string `json:"openQuestions"`
	Confidence      string   `json:"confidence"` // high / medium / low
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
//   - namespace 必须和任务一致，禁止跨 namespace 查，更禁止 kube-system
//     这类系统命名空间；
//   - 资源名不能为空，不能含 / 和 ..，防止路径穿越式的滥用。
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
	ns := call.Args["namespace"]
	if ns == "" {
		return fmt.Errorf("缺少 namespace 参数")
	}
	if ns != namespace {
		return fmt.Errorf("目标 namespace %q 与任务 %q 不一致，拒绝执行", ns, namespace)
	}
	if ns == "kube-system" || ns == "kube-public" || ns == "kube-node-lease" {
		return fmt.Errorf("禁止查询系统命名空间 %q", ns)
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
			if a.OnStep != nil {
				_ = a.OnStep(ctx, &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					Summary: "工具调用被拒绝：" + err.Error(), Error: err.Error(),
				})
			}
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
			if a.OnStep != nil {
				_ = a.OnStep(ctx, &store.RunStep{
					RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
					CallID: call.ID, ArgsJSON: mustJSON(call.Args),
					Summary: "工具执行失败：" + execErr.Error(), Error: execErr.Error(),
					DurationMs: duration.Milliseconds(),
				})
			}
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
		if a.OnStep != nil {
			resultJSON, _ := json.Marshal(out.Data)
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
		priorSteps = append(priorSteps, store.RunStep{
			RunID: run.ID, Kind: "tool_result", ToolName: call.Name,
			CallID: call.ID, ArgsJSON: mustJSON(call.Args), Summary: out.Summary,
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
