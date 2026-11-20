package agent

import (
	"context"
	"testing"

	"kubecause/internal/store"
)

// loopModel 永远返回工具调用，从不交报告，用于测步数上限。
type loopModel struct{}

func (loopModel) Next(ctx context.Context, in ModelInput) (ModelStep, error) {
	return ModelStep{ToolCall: &ToolCall{ID: "call-x", Name: "get_pod", Args: map[string]string{"namespace": in.Namespace, "pod": "demo-api-7d9f6c4b5-xk2v9"}}}, nil
}

type stubTools struct{ calls int }

func (s *stubTools) GetPod(ctx context.Context, ns, name string) (ToolOutput, error) {
	s.calls++
	return ToolOutput{Summary: "Pod ok", Data: map[string]string{"pod": name}}, nil
}
func (s *stubTools) ListWorkloadPods(ctx context.Context, ns, dep string) (ToolOutput, error) {
	return ToolOutput{Summary: "pods", Data: map[string]any{}}, nil
}
func (s *stubTools) GetEvents(ctx context.Context, ns, name string) (ToolOutput, error) {
	return ToolOutput{Summary: "events", Data: map[string]any{}}, nil
}
func (s *stubTools) GetPodLogs(ctx context.Context, ns, pod, container string, previous bool) (ToolOutput, error) {
	return ToolOutput{Summary: "logs", Data: map[string]any{}}, nil
}
func (s *stubTools) GetDeployment(ctx context.Context, ns, name string) (ToolOutput, error) {
	return ToolOutput{Summary: "deploy", Data: map[string]any{}}, nil
}
func (s *stubTools) SearchRunbooks(ctx context.Context, q string) (ToolOutput, error) {
	return ToolOutput{Summary: "runbook", Data: map[string]any{}}, nil
}

func TestStepLimit(t *testing.T) {
	tools := &stubTools{}
	ag := &Agent{Tools: tools, Model: loopModel{}}
	run := &store.Run{ID: "run_test", Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启", MaxSteps: 3, TimeoutSeconds: 60}
	report, err := ag.Run(context.Background(), run, nil)
	if err != nil {
		t.Fatalf("Agent 不应在步数耗尽时报错: %v", err)
	}
	if report == nil || report.Confidence != "low" {
		t.Fatalf("步数耗尽应返回证据不足的低置信报告: %+v", report)
	}
	// loopModel 每次返回同一个调用，指纹去重后只执行一次；
	// 关键是循环必须在 MaxSteps 内结束，不会无限跑。
	if tools.calls > 3 {
		t.Fatalf("工具调用超过上限: %d", tools.calls)
	}
}

func TestValidateToolCallRejectsIllegalTarget(t *testing.T) {
	cases := []struct {
		name string
		call *ToolCall
	}{
		{"未知工具", &ToolCall{Name: "delete_pod", Args: map[string]string{"namespace": "demo", "pod": "x"}}},
		{"跨 namespace", &ToolCall{Name: "get_pod", Args: map[string]string{"namespace": "prod", "pod": "x"}}},
		{"系统命名空间", &ToolCall{Name: "get_pod", Args: map[string]string{"namespace": "kube-system", "pod": "x"}}},
		{"空目标名", &ToolCall{Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": ""}}},
		{"路径穿越", &ToolCall{Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "../x"}}},
		{"runbook 缺 query", &ToolCall{Name: "search_runbooks", Args: map[string]string{}}},
	}
	for _, tc := range cases {
		if err := ValidateToolCall("demo", tc.call); err == nil {
			t.Errorf("%s：应被拒绝，但通过了", tc.name)
		}
	}
	if err := ValidateToolCall("demo", &ToolCall{Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-7d9f6c4b5-xk2v9"}}); err != nil {
		t.Errorf("合法调用应通过: %v", err)
	}
}

func TestValidateReportFiltersForeignEvidence(t *testing.T) {
	rep := &Report{Facts: []string{"a"}, Cause: "x", EvidenceIDs: []string{"ev-1", "ev-999", "call-1"}, Confidence: "high"}
	ValidateReport(rep, map[string]bool{"ev-1": true, "call-1": true})
	if len(rep.EvidenceIDs) != 2 {
		t.Fatalf("应过滤掉不属于当前任务的证据 ev-999，得到 %+v", rep.EvidenceIDs)
	}
}

func TestTruncateLog(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "line\n"
	}
	out, truncated := TruncateLog(long)
	if !truncated {
		t.Fatalf("超长日志应被截断")
	}
	if len(out) == 0 || len(out) >= len(long) {
		t.Fatalf("截断后长度不对")
	}
}
