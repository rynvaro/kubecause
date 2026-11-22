package agent

import (
	"context"
	"testing"

	"kubecause/internal/store"
)

// batchModel 第一轮一次返回 2 个工具调用，第二轮才交报告，
// 用来验证 Agent 一轮里把一批调用全执行了、结果都回传给模型。
type batchModel struct {
	rounds      int
	seenHistory int
}

func (m *batchModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	m.rounds++
	if m.rounds == 1 {
		return ModelStep{ToolCalls: []*ToolCall{
			{ID: "call-1", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}},
			{ID: "call-2", Name: "get_events", Args: map[string]string{"namespace": "demo", "name": "demo-api-7d9f6c4b5-xk2v9"}},
		}}, nil
	}
	m.seenHistory = len(in.History)
	return ModelStep{IsFinal: true, Report: &Report{
		Facts:           []string{"Pod OOMKilled"},
		Cause:           "容器内存限制过低",
		EvidenceIDs:     []string{"ev-1", "ev-2"},
		Recommendations: []string{"调高 limit"},
		OpenQuestions:   []string{},
		Confidence:      "medium",
	}}, nil
}

// recordingTools 按调用顺序记下工具名，返回固定结果。
type recordingTools struct {
	calls []string
}

func (s *recordingTools) record(name, summary string) (ToolOutput, error) {
	s.calls = append(s.calls, name)
	return ToolOutput{Summary: summary, Data: map[string]any{}}, nil
}

func (s *recordingTools) GetPod(ctx context.Context, ns, name string) (ToolOutput, error) {
	return s.record("get_pod", "Pod demo-api-7d9f6c4b5-xk2v9 OOMKilled")
}
func (s *recordingTools) ListWorkloadPods(ctx context.Context, ns, dep string) (ToolOutput, error) {
	return s.record("list_workload_pods", "pods")
}
func (s *recordingTools) GetEvents(ctx context.Context, ns, name string) (ToolOutput, error) {
	return s.record("get_events", "events")
}
func (s *recordingTools) GetPodLogs(ctx context.Context, ns, pod, container string, previous bool) (ToolOutput, error) {
	return s.record("get_pod_logs", "logs")
}
func (s *recordingTools) GetDeployment(ctx context.Context, ns, name string) (ToolOutput, error) {
	return s.record("get_deployment", "deploy")
}
func (s *recordingTools) SearchRunbooks(ctx context.Context, q string) (ToolOutput, error) {
	return s.record("search_runbooks", "runbook")
}
func (s *recordingTools) ListNamespaces(ctx context.Context) (ToolOutput, error) {
	return s.record("list_namespaces", "namespaces")
}
func (s *recordingTools) ListDeployments(ctx context.Context, ns string) (ToolOutput, error) {
	return s.record("list_deployments", "deployments")
}
func (s *recordingTools) ListProblemPods(ctx context.Context, ns string) (ToolOutput, error) {
	return s.record("list_problem_pods", "problem pods")
}
func (s *recordingTools) ListPods(ctx context.Context, ns string) (ToolOutput, error) {
	return s.record("list_pods", "pods")
}
func (s *recordingTools) GetClusterOverview(ctx context.Context) (ToolOutput, error) {
	return s.record("get_cluster_overview", "overview")
}

func TestAgentExecutesAllToolCallsInOneBatch(t *testing.T) {
	// 回归：模型一次返回多个工具调用时，Agent 一轮里按顺序全执行，
	// 不能只执行第一个、其余静默丢掉。
	tools := &recordingTools{}
	m := &batchModel{}
	ag := &Agent{Tools: tools, Model: m}
	run := &store.Run{ID: "run_multi", Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启", MaxSteps: 3, TimeoutSeconds: 60}
	report, err := ag.Run(context.Background(), run, nil)
	if err != nil {
		t.Fatalf("Agent 不应报错: %v", err)
	}
	if report == nil || report.Cause != "容器内存限制过低" {
		t.Fatalf("应拿到第二轮交的报告，得到 %+v", report)
	}
	if m.rounds != 2 {
		t.Fatalf("一批 2 个调用 + 交报告应只用 2 轮模型，实际 %d 轮", m.rounds)
	}
	if len(tools.calls) != 2 || tools.calls[0] != "get_pod" || tools.calls[1] != "get_events" {
		t.Fatalf("一批里的 2 个调用应按顺序全执行，得到 %v", tools.calls)
	}
	if m.seenHistory != 2 {
		t.Fatalf("第二轮模型应看到 2 条工具结果，实际 %d 条", m.seenHistory)
	}
}
