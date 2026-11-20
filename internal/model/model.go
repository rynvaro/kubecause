// Package model 提供模型适配器。
//
// Slice 1 只有桩模型：按固定顺序走一遍工具调用，最后根据证据给诊断。
// 这样做的好处是演示和测试完全确定，不依赖外部模型 API、
// 不花钱、不断网也能跑。真模型在 Slice 2 接入，接口已经留好，
// Agent 不需要改，只需要换一个 Model 实现。
package model

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"kubecause/internal/agent"
)

// Stub 是确定性桩模型。
//
// 它按 StepIndex 决定下一步：
//   0: list_workload_pods（Deployment 任务）或 get_pod（Pod 任务）
//   1: get_pod
//   2: get_events
//   3: get_pod_logs（当前日志）
//   4: get_deployment
//   5: search_runbooks，关键词取自已看到的证据（优先 OOMKilled）
//   6: 交最终报告
//
// 如果是重启恢复，StepIndex 会从已用步数继续，不会从头重走。
type Stub struct{}

// NewStub 返回桩模型。
func NewStub() *Stub { return &Stub{} }

// Next 按 StepIndex 返回下一步。
func (s *Stub) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	ns := in.Namespace
	// 从历史里找 Pod 名：list_workload_pods 的结果里有第一个 Pod，
	// 桩模型先用任务名拼一个默认 Pod 名，拿到真实列表后再纠正。
	// fake 客户端预置的 Pod 名是 demo-api-7d9f6c4b5-xk2v9，见 internal/kube。
	podName := discoverPodName(in)
	deployName := in.Name
	if in.Kind == "Pod" {
		podName = in.Name
		deployName = "demo-api"
	}
	call := func(name string, args map[string]string) agent.ModelStep {
		if args == nil {
			args = map[string]string{}
		}
		args["namespace"] = ns
		return agent.ModelStep{
			ToolCall: &agent.ToolCall{ID: fmt.Sprintf("call-%d", in.StepIndex+1), Name: name, Args: args},
		}
	}
	switch in.StepIndex {
	case 0:
		if in.Kind == "Deployment" {
			return call("list_workload_pods", map[string]string{"deployment": deployName}), nil
		}
		return call("get_pod", map[string]string{"pod": podName}), nil
	case 1:
		return call("get_pod", map[string]string{"pod": podName}), nil
	case 2:
		return call("get_events", map[string]string{"name": podName}), nil
	case 3:
		return call("get_pod_logs", map[string]string{"pod": podName, "container": "demo-api", "previous": "false"}), nil
	case 4:
		return call("get_deployment", map[string]string{"deployment": deployName}), nil
	case 5:
		// 关键词优先用已看到的 OOMKilled，其次用任务问题里的关键词。
		kw := "OOMKilled"
		for _, h := range in.History {
			if strings.Contains(h.Summary, "OOMKilled") || strings.Contains(h.ResultJSON, "OOMKilled") {
				kw = "OOMKilled"
				break
			}
		}
		if kw == "" {
			kw = in.Question
		}
		return call("search_runbooks", map[string]string{"query": kw}), nil
	default:
		return agent.ModelStep{IsFinal: true, Report: s.buildReport(in)}, nil
	}
}

func discoverPodName(in agent.ModelInput) string {
	// 默认就是 fake 预置的 Pod 名；如果历史里已经列出过 Pod，
	// 从结果 JSON 里尽量解析出第一个 pod 名。
	for _, h := range in.History {
		if h.ToolName == "list_workload_pods" && h.ResultJSON != "" {
			// 结果是 {"pods":["demo-api-..."]} 这种简单结构，直接抽字符串。
			for _, part := range strings.FieldsFunc(h.ResultJSON, func(r rune) bool { return r == '"' || r == ',' || r == '[' || r == ']' || r == '{' || r == '}' }) {
				if strings.HasPrefix(part, "demo-api-") {
					return part
				}
			}
		}
	}
	return "demo-api-7d9f6c4b5-xk2v9"
}

// buildReport 根据已收集的证据拼最终报告。
// 桩模型不编证据：只引用 ModelInput.Evidence 里真实存在的 ID，
// 并且只有看到 OOMKilled 才下“内存限制过低”的结论，
// 否则诚实地说证据不足。
func (s *Stub) buildReport(in agent.ModelInput) *agent.Report {
	ids := make([]string, 0, len(in.Evidence))
	for id := range in.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	seenOOM := false
	var facts []string
	for _, h := range in.History {
		if h.Summary != "" {
			facts = append(facts, h.Summary)
		}
		if strings.Contains(h.Summary, "OOMKilled") || strings.Contains(h.ResultJSON, "OOMKilled") {
			seenOOM = true
		}
	}
	if len(facts) == 0 {
		facts = []string{"未收集到有效证据"}
	}
	if seenOOM {
		return &agent.Report{
			Facts:       facts,
			Cause:       "容器内存限制过低，触发 OOMKilled 导致反复重启",
			EvidenceIDs: ids,
			Recommendations: []string{
				"将 demo-api 容器的内存 limit 从 128Mi 调高到 256Mi（需人工审批后执行，Slice 3）",
				"检查应用是否存在内存泄漏，确认 256Mi 是否足够",
				"观察调高后的重启次数是否停止增长",
			},
			OpenQuestions: []string{"应用本身是否存在内存泄漏，仍需进一步确认"},
			Confidence:    "high",
		}
	}
	return &agent.Report{
		Facts:           facts,
		Cause:           "证据不足，无法确定根因",
		EvidenceIDs:     ids,
		Recommendations: []string{"补查 Pod 状态、事件和日志后再下结论"},
		OpenQuestions:   []string{"未观察到 OOMKilled 或其他明确故障信号，需要更多证据"},
		Confidence:      "low",
	}
}

// Real 是真模型的占位实现，Slice 1 不做假调用。
// Slice 2 在这里接具体的模型 API（OpenAI / DeepSeek 兼容协议），
// 负责把模型返回的工具调用映射成 agent.ToolCall。
type Real struct {
	APIKey string
}

// NewReal 返回真模型占位。调用 Next 会明确报错，不会偷偷调一个假接口。
func NewReal(apiKey string) *Real { return &Real{APIKey: apiKey} }

// Next 在 Slice 1 明确返回未实现。
func (r *Real) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	return agent.ModelStep{}, fmt.Errorf("MODEL_MODE=real 在 Slice 1 未实现，请在 Slice 2 接入真实模型 API，或先用 MODEL_MODE=stub")
}
