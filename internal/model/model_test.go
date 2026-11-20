package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/store"
)

// mockOpenAI 返回一个按 mode 决定行为的 OpenAI 兼容服务。
// mode=toolcall 返回一次 get_pod 调用；mode=diagnosis 返回 submit_diagnosis；
// mode=illegal 返回 delete_pod；mode=flaky 第一次 500 第二次成功。
func mockOpenAI(t *testing.T, mode string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		if mode == "flaky" && *calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		if mode == "badrequest" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
			return
		}
		var resp string
		switch mode {
		case "toolcall", "flaky":
			resp = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}}]}}]}`
		case "diagnosis":
			resp = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-9","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"Pod 重启 5 次\"],\"cause\":\"容器内存限制过低，触发 OOMKilled\",\"evidenceIds\":[\"ev-1\"],\"recommendations\":[\"调高 limit\"],\"openQuestions\":[],\"confidence\":\"high\"}"}}]}}]}`
		case "illegal":
			resp = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"delete_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"x\"}"}}]}}]}`
		case "text":
			resp = `{"choices":[{"message":{"role":"assistant","content":"容器内存限制过低，触发 OOMKilled"}}]}`
		default:
			resp = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, resp)
	}))
}

func testInput() agent.ModelInput {
	return agent.ModelInput{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启", StepIndex: 0, Evidence: map[string]string{"ev-1": "Pod 重启 5 次"}}
}

func TestRealMapsToolCall(t *testing.T) {
	calls := 0
	srv := mockOpenAI(t, "toolcall", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if step.ToolCall == nil || step.ToolCall.Name != "get_pod" {
		t.Fatalf("应映射成 get_pod 调用，得到 %+v", step.ToolCall)
	}
	if step.ToolCall.Args["pod"] != "demo-api-7d9f6c4b5-xk2v9" || step.ToolCall.Args["namespace"] != "demo" {
		t.Fatalf("参数映射不对: %+v", step.ToolCall.Args)
	}
	if calls != 1 {
		t.Fatalf("应只调一次模型，实际 %d 次", calls)
	}
}

func TestRealMapsSubmitDiagnosis(t *testing.T) {
	calls := 0
	srv := mockOpenAI(t, "diagnosis", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("submit_diagnosis 应映射成最终报告，得到 %+v", step)
	}
	if !strings.Contains(step.Report.Cause, "OOMKilled") || step.Report.Confidence != "high" {
		t.Fatalf("报告内容不对: %+v", step.Report)
	}
}

func TestRealIllegalToolCallIsRejectedByAgent(t *testing.T) {
	// 真模型适配器不自己吞掉非法调用，原样交给 Agent，
	// Agent 的 ValidateToolCall 负责拒绝并记一步错误，循环继续。
	// 这里验证两层：Real 原样返回，Agent 拒绝。
	calls := 0
	srv := mockOpenAI(t, "illegal", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Real 不应在这里吞掉非法调用: %v", err)
	}
	if step.ToolCall == nil || step.ToolCall.Name != "delete_pod" {
		t.Fatalf("应原样返回 delete_pod，得到 %+v", step.ToolCall)
	}
	if err := agent.ValidateToolCall("demo", step.ToolCall); err == nil {
		t.Fatalf("Agent 应拒绝 delete_pod")
	}
}

func TestRealRetriesOn500ThenSucceeds(t *testing.T) {
	calls := 0
	srv := mockOpenAI(t, "flaky", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second, MaxRetries: 3})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if step.ToolCall == nil {
		t.Fatalf("重试后应拿到工具调用")
	}
	if calls != 2 {
		t.Fatalf("第一次 500 后应重试一次，共 2 次，实际 %d 次", calls)
	}
}

func TestRealNoRetryOn400(t *testing.T) {
	calls := 0
	srv := mockOpenAI(t, "badrequest", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second, MaxRetries: 3})
	if _, err := m.Next(context.Background(), testInput()); err == nil {
		t.Fatalf("400 不应重试，应直接报错")
	}
	if calls != 1 {
		t.Fatalf("400 只应调一次，实际 %d 次", calls)
	}
}

func TestRealMissingKey(t *testing.T) {
	m := NewReal("")
	if _, err := m.Next(context.Background(), testInput()); err == nil {
		t.Fatalf("缺 Key 应明确报错")
	}
}

func TestRealTextFallback(t *testing.T) {
	calls := 0
	srv := mockOpenAI(t, "text", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if !step.IsFinal || step.Report == nil || !strings.Contains(step.Report.Cause, "OOMKilled") {
		t.Fatalf("纯文本应回退成最终报告，得到 %+v", step.Report)
	}
}

func TestStubProbeNotMisjudgedAsImagePull(t *testing.T) {
	// 回归测试：探针案的一手证据是 Liveness probe failed，
	// 即使 search_runbooks 带回了镜像手册，结论也必须是探针失败，
	// 不能被检索片段压过。Slice 2 之前这里误判成镜像拉取。
	s := NewStub()
	rep := s.buildReport(agent.ModelInput{
		Namespace: "demo", Kind: "Deployment", Name: "demo-api",
		History: []store.RunStep{
			{ToolName: "get_events", Summary: "查到 2 条相关事件：Unhealthy, Unhealthy", ResultJSON: `{"events":[{"reason":"Unhealthy","message":"Liveness probe failed: HTTP probe failed"}]}`},
			{ToolName: "get_pod_logs", Summary: "日志中出现 Liveness probe failed", ResultJSON: `{"log":"liveness probe failed"}`},
			{ToolName: "search_runbooks", Summary: "检索命中手册：探针失败；镜像拉取失败", ResultJSON: `{"snippets":[{"title":"镜像拉取失败 ImagePullBackOff"}]}`},
		},
		Evidence: map[string]string{"ev-1": "x"},
	})
	if !strings.Contains(rep.Cause, "探针") {
		t.Fatalf("探针案的结论应包含探针，得到 %q", rep.Cause)
	}
}

func TestParseToolArgsBool(t *testing.T) {
	args, err := parseToolArgs(`{"pod":"x","previous":true}`)
	if err != nil || args["previous"] != "true" {
		t.Fatalf("布尔参数应转成字符串 true，得到 %+v err=%v", args, err)
	}
}

func TestTruncateRunes(t *testing.T) {
	s := strings.Repeat("a", 100)
	if got := truncateRunes(s, 10); len([]rune(got)) > 20 {
		t.Fatalf("截断后应变短，得到长度 %d", len(got))
	}
	// 确保 JSON 编码工具定义时不报错。
	b, err := json.Marshal(toolDefs())
	if err != nil || len(b) == 0 {
		t.Fatalf("工具定义应能编码成 JSON: %v", err)
	}
}
