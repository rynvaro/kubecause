package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/mcpclient"
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
		case "multicall":
			resp = `{"choices":[{"message":{"role":"assistant","content":"先查 Pod 和事件。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}},{"id":"call-2","type":"function","function":{"name":"get_events","arguments":"{\"namespace\":\"demo\",\"name\":\"demo-api-7d9f6c4b5-xk2v9\"}"}}]}}]}`
		case "diagmixed":
			resp = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}},{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"Pod 重启 5 次\"],\"cause\":\"容器内存限制过低，触发 OOMKilled\",\"evidenceIds\":[\"ev-1\"],\"recommendations\":[\"调高 limit\"],\"openQuestions\":[],\"confidence\":\"high\"}"}}]}}]}`
		case "mixedns":
			// 4 个调用都不带 namespace：node_list（MCP 工具）、get_pod（内置）、
			// search_runbooks（无 namespace 参数）、list_namespaces（无 namespace 参数）。
			resp = `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"node_list","arguments":"{}"}},{"id":"call-2","type":"function","function":{"name":"get_pod","arguments":"{\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}},{"id":"call-3","type":"function","function":{"name":"search_runbooks","arguments":"{\"query\":\"OOMKilled\"}"}},{"id":"call-4","type":"function","function":{"name":"list_namespaces","arguments":"{}"}}]}}]}`
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
	if len(step.ToolCalls) != 1 || step.ToolCalls[0].Name != "get_pod" {
		t.Fatalf("应映射成 get_pod 调用，得到 %+v", step.ToolCalls)
	}
	if step.ToolCalls[0].Args["pod"] != "demo-api-7d9f6c4b5-xk2v9" || step.ToolCalls[0].Args["namespace"] != "demo" {
		t.Fatalf("参数映射不对: %+v", step.ToolCalls[0].Args)
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
	if len(step.ToolCalls) != 1 || step.ToolCalls[0].Name != "delete_pod" {
		t.Fatalf("应原样返回 delete_pod，得到 %+v", step.ToolCalls)
	}
	if err := agent.ValidateToolCall("demo", step.ToolCalls[0]); err == nil {
		t.Fatalf("Agent 应拒绝 delete_pod")
	}
}

func TestRealMapsMultipleToolCalls(t *testing.T) {
	// 回归：模型一次返回多个工具调用时，全部按返回顺序映射回来，
	// 不能只取第一个、其余静默丢掉。
	calls := 0
	srv := mockOpenAI(t, "multicall", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if step.IsFinal {
		t.Fatalf("没交报告时不应是最终步，得到 %+v", step)
	}
	if len(step.ToolCalls) != 2 {
		t.Fatalf("一次返回 2 个调用，应映射成 2 个，得到 %d 个", len(step.ToolCalls))
	}
	if step.ToolCalls[0].Name != "get_pod" || step.ToolCalls[1].Name != "get_events" {
		t.Fatalf("调用顺序应和模型返回一致，得到 %v", step.ToolCalls)
	}
	if step.ToolCalls[0].Args["pod"] != "demo-api-7d9f6c4b5-xk2v9" || step.ToolCalls[1].Args["name"] != "demo-api-7d9f6c4b5-xk2v9" {
		t.Fatalf("参数映射不对: %+v / %+v", step.ToolCalls[0].Args, step.ToolCalls[1].Args)
	}
}

func TestRealChatMapsMultipleToolCalls(t *testing.T) {
	// Chat 路同样：一批调用全映射回来，顺带的文字也保留。
	calls := 0
	srv := mockOpenAI(t, "multicall", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	st, err := m.Chat(context.Background(), []ChatMessage{{Role: "system", Content: "x"}, {Role: "user", Content: "当前状态怎么样"}})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if st.IsFinal {
		t.Fatalf("没交报告时不应是最终步，得到 %+v", st)
	}
	if len(st.ToolCalls) != 2 {
		t.Fatalf("一次返回 2 个调用，应映射成 2 个，得到 %d 个", len(st.ToolCalls))
	}
	if st.ToolCalls[0].Name != "get_pod" || st.ToolCalls[1].Name != "get_events" {
		t.Fatalf("调用顺序应和模型返回一致，得到 %v", st.ToolCalls)
	}
	if st.Content != "先查 Pod 和事件。" {
		t.Fatalf("顺带的文字应保留，得到 %q", st.Content)
	}
}

func TestRealSubmitDiagnosisWinsOverQueryCalls(t *testing.T) {
	// submit_diagnosis 是终结信号：同一批里只要出现它，只处理它，
	// 跳过同批其他查询调用——不管它在第几个位置。
	calls := 0
	srv := mockOpenAI(t, "diagmixed", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("一批里有 submit_diagnosis 时应直接交报告，得到 %+v", step)
	}
	if len(step.ToolCalls) != 0 {
		t.Fatalf("交报告时同批查询调用应跳过，得到 %+v", step.ToolCalls)
	}
	if !strings.Contains(step.Report.Cause, "OOMKilled") {
		t.Fatalf("报告内容不对: %+v", step.Report)
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
	if len(step.ToolCalls) == 0 {
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

// TestToolDescriptionsDescribeOnly 覆盖 Slice 8 的“工具描述只描述、不禁止”：
// 每个工具的 description 只能写它返回什么、覆盖多大范围，
// 不能写“不要调 xxx”“不再需要调 xxx”这种禁止句。
// 禁止句是替模型做决策：模型该调哪个工具，应由它按返回内容自己判断。
// 同时要求发现工具的描述讲清“不带 namespace 时查全部、带时只查那个”。
func TestToolDescriptionsDescribeOnly(t *testing.T) {
	defs := toolDefs()
	if len(defs) != 12 {
		t.Fatalf("工具表应有 12 个定义（11 个工具 + submit_diagnosis），得到 %d", len(defs))
	}
	banned := []string{"不要调", "不再需要", "不需要调", "不要查", "不要再调", "禁止调"}
	for _, d := range defs {
		desc := d.Function.Description
		if strings.TrimSpace(desc) == "" {
			t.Fatalf("工具 %s 的描述不能为空", d.Function.Name)
		}
		for _, b := range banned {
			if strings.Contains(desc, b) {
				t.Fatalf("工具 %s 的描述里不应有禁止句 %q，得到 %q", d.Function.Name, b, desc)
			}
		}
		for _, p := range d.Function.Parameters["properties"].(map[string]any) {
			if pm, ok := p.(map[string]any); ok {
				if ds, ok := pm["description"].(string); ok {
					for _, b := range banned {
						if strings.Contains(ds, b) {
							t.Fatalf("工具 %s 的参数描述里不应有禁止句 %q，得到 %q", d.Function.Name, b, ds)
						}
					}
				}
			}
		}
	}
	// 发现工具必须讲清返回范围：总览一次返回全集群汇总，
	// 问题 Pod 不带 namespace 时查全部、带时只查那个。
	byName := map[string]string{}
	for _, d := range defs {
		byName[d.Function.Name] = d.Function.Description
	}
	if !strings.Contains(byName["get_cluster_overview"], "全集群") {
		t.Fatalf("get_cluster_overview 应讲清一次返回全集群汇总，得到 %q", byName["get_cluster_overview"])
	}
	if !strings.Contains(byName["list_problem_pods"], "全部命名空间") {
		t.Fatalf("list_problem_pods 应讲清不带 namespace 时查全部命名空间，得到 %q", byName["list_problem_pods"])
	}
	if !strings.Contains(byName["list_pods"], "全部命名空间") {
		t.Fatalf("list_pods 应讲清不带 namespace 时查全部命名空间，得到 %q", byName["list_pods"])
	}
	if !strings.Contains(byName["list_namespaces"], "只返回名字") {
		t.Fatalf("list_namespaces 应讲清只返回名字列表，得到 %q", byName["list_namespaces"])
	}
	if !strings.Contains(byName["list_deployments"], "不返回 Pod 详情") {
		t.Fatalf("list_deployments 应讲清不返回 Pod 详情，得到 %q", byName["list_deployments"])
	}
}

func TestRealExtraTools(t *testing.T) {
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: "http://example.com", ModelName: "test-model", HTTPClient: http.DefaultClient, Timeout: 5 * time.Second})
	// 默认 12 个工具（11 个 k8s 工具 + submit_diagnosis）。
	if got := len(m.toolDefs()); got != 12 {
		t.Fatalf("默认工具表 = %d 个, want 12", got)
	}
	// MCP 发现的节点工具并进来，排在后面，原来的 12 个不动。
	m.SetExtraTools([]mcpclient.ToolDef{
		{Name: "node_list", Description: "列节点", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
		{Name: "node_disk", Description: "磁盘", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}},
	})
	defs := m.toolDefs()
	if len(defs) != 14 {
		t.Fatalf("加了 2 个节点工具后工具表 = %d 个, want 14", len(defs))
	}
	if defs[12].Function.Name != "node_list" || defs[13].Function.Name != "node_disk" {
		t.Errorf("节点工具没有排在后面: %v", defs[12:])
	}
	// 传空恢复成默认表——没有 MCP Client 的部署不受影响。
	m.SetExtraTools(nil)
	if got := len(m.toolDefs()); got != 12 {
		t.Errorf("清空后工具表 = %d 个, want 12", got)
	}
}

func TestRealNamespaceBackfillOnlyForBuiltinTools(t *testing.T) {
	// issue #11：namespace 只补给 schema 里声明了它的内置工具。
	// MCP 发现的 node_* 工具不补——Server 会拒绝 schema 之外的字段；
	// search_runbooks / list_namespaces 的 schema 本来就没有 namespace，也不补。
	calls := 0
	srv := mockOpenAI(t, "mixedns", &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
	m.SetExtraTools([]mcpclient.ToolDef{{Name: "node_list", Description: "列出节点", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}})
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if len(step.ToolCalls) != 4 {
		t.Fatalf("应返回 4 个调用，得到 %d", len(step.ToolCalls))
	}
	byName := map[string]map[string]string{}
	for _, c := range step.ToolCalls {
		byName[c.Name] = c.Args
	}
	if _, ok := byName["node_list"]["namespace"]; ok {
		t.Fatalf("node_list 不应被补 namespace，得到 %+v", byName["node_list"])
	}
	if byName["get_pod"]["namespace"] != "demo" {
		t.Fatalf("get_pod 应被补上任务的 namespace，得到 %+v", byName["get_pod"])
	}
	if _, ok := byName["search_runbooks"]["namespace"]; ok {
		t.Fatalf("search_runbooks 不应被补 namespace，得到 %+v", byName["search_runbooks"])
	}
	if _, ok := byName["list_namespaces"]["namespace"]; ok {
		t.Fatalf("list_namespaces 的 schema 没有 namespace，不应补，得到 %+v", byName["list_namespaces"])
	}
}

// scriptedOpenAI 按调用次数顺序返回写好的响应体，并把每次的请求体记下来。
// responses[i] 是第 i+1 次调用的返回；调用次数超过表长时返回最后一项。
// 用来验证真模型路径在“第一次返回坏报告、第二次纠正”这类剧本下的行为。
func scriptedOpenAI(t *testing.T, responses []string, calls *int) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		idx := *calls - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responses[idx])
	}))
	return srv, &bodies
}

func newTestReal(t *testing.T, srv *httptest.Server) *Real {
	t.Helper()
	return NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second})
}

// requestMessage 是 chat completions 请求体里的一条消息（测试用）。
type requestMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// requestMessages 把一次 chat completions 请求体里的 messages 数组解出来，
// 用来断言纠错时追加的是否正好是一对 assistant+tool 消息。
func requestMessages(t *testing.T, body string) []requestMessage {
	t.Helper()
	var req struct {
		Messages []requestMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	return req.Messages
}

func submitDiagnosisResp(callID, args string) string {
	return `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"submit_diagnosis","arguments":` + strconv.Quote(args) + `}}]}}]}`
}

// submitDiagnosisRespWithContent 和 submitDiagnosisResp 一样，
// 只是模型同时还返回了一段文字（content）。
func submitDiagnosisRespWithContent(callID, content, args string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + strconv.Quote(content) + `,"tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"submit_diagnosis","arguments":` + strconv.Quote(args) + `}}]}}]}`
}

func TestParseDiagnosisArgsStringFacts(t *testing.T) {
	// issue #13 的最小复现：facts 是字符串而不是数组。
	// 截图里的报错就是这一句，修好后应兼容成单元素数组、不再报错。
	rep, err := parseDiagnosisArgs(`{"facts":"节点状态正常","cause":"节点状态正常，Pod 未受节点影响","evidenceIds":[],"confidence":"low"}`)
	if err != nil {
		t.Fatalf("facts 为字符串时应兼容，不应报错: %v", err)
	}
	if len(rep.Facts) != 1 || rep.Facts[0] != "节点状态正常" {
		t.Fatalf("facts 应兼容成 [\"节点状态正常\"]，得到 %+v", rep.Facts)
	}
	if rep.Confidence != "low" {
		t.Fatalf("confidence 应保持 low，得到 %q", rep.Confidence)
	}
	// 其他展示用数组字段同样兼容单个字符串。
	rep, err = parseDiagnosisArgs(`{"facts":["a"],"cause":"c","evidenceIds":"ev-1","recommendations":"调高 limit","openQuestions":"还有疑问吗","confidence":"medium"}`)
	if err != nil {
		t.Fatalf("其他数组字段为字符串时也应兼容: %v", err)
	}
	if len(rep.EvidenceIDs) != 1 || rep.EvidenceIDs[0] != "ev-1" {
		t.Fatalf("evidenceIds 应兼容成 [\"ev-1\"]，得到 %+v", rep.EvidenceIDs)
	}
	if len(rep.Recommendations) != 1 || rep.Recommendations[0] != "调高 limit" {
		t.Fatalf("recommendations 应兼容成单元素数组，得到 %+v", rep.Recommendations)
	}
	if len(rep.OpenQuestions) != 1 || rep.OpenQuestions[0] != "还有疑问吗" {
		t.Fatalf("openQuestions 应兼容成单元素数组，得到 %+v", rep.OpenQuestions)
	}
}

func TestParseDiagnosisArgsIncompatibleTypes(t *testing.T) {
	// 完全不兼容的类型：必须返回指明字段名的错误，不要一律 fmt.Sprint 糊过去。
	cases := []struct {
		args string
		want string
	}{
		{`{"facts":123,"cause":"c"}`, "facts 应为字符串数组，收到 number"},
		{`{"facts":{"a":"b"},"cause":"c"}`, "facts 应为字符串数组，收到 object"},
		{`{"facts":true,"cause":"c"}`, "facts 应为字符串数组，收到 boolean"},
		{`{"facts":["a",5],"cause":"c"}`, "facts 数组元素应为字符串"},
		{`{"facts":["a"],"cause":123}`, "cause 应为字符串，收到 number"},
		{`{"facts":["a"],"cause":"c","confidence":1}`, "confidence 应为字符串，收到 number"},
	}
	for _, c := range cases {
		_, err := parseDiagnosisArgs(c.args)
		if err == nil {
			t.Fatalf("args=%s 应报错", c.args)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("args=%s 错误应指明字段，want 包含 %q，得到 %v", c.args, c.want, err)
		}
	}
}

func TestParseDiagnosisArgsDropsInvalidRepair(t *testing.T) {
	// suggestedRepair / suggestedCreation 严格校验：解析失败时丢弃该建议，
	// 报告其余部分照常返回，绝不生成审批卡片。
	rep, err := parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":"not-an-object"}`)
	if err != nil {
		t.Fatalf("修复建议非法时报告本身仍应返回: %v", err)
	}
	if rep.SuggestedRepair != nil {
		t.Fatalf("非法的修复建议应被丢弃，得到 %+v", rep.SuggestedRepair)
	}
	if len(rep.OpenQuestions) == 0 || !strings.Contains(strings.Join(rep.OpenQuestions, ""), "修复建议格式无效") {
		t.Fatalf("丢弃修复建议时应在 openQuestions 里说明，得到 %+v", rep.OpenQuestions)
	}
	rep, err = parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedCreation":{"namespace":"demo","name":"x","image":"nginx","replicas":"3"}}`)
	if err != nil {
		t.Fatalf("创建建议非法时报告本身仍应返回: %v", err)
	}
	if rep.SuggestedCreation != nil {
		t.Fatalf("非法的创建建议应被丢弃，得到 %+v", rep.SuggestedCreation)
	}
	// 合法的修复建议不受影响：照常保留。
	rep, err = parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","container":"demo-api","reason":"内存不足","patches":[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"256Mi"}]}}`)
	if err != nil {
		t.Fatalf("合法的修复建议应能解析: %v", err)
	}
	if rep.SuggestedRepair == nil || len(rep.SuggestedRepair.Patches) != 1 {
		t.Fatalf("合法的修复建议应保留，得到 %+v", rep.SuggestedRepair)
	}
}

func TestParseDiagnosisArgsDropsIncompleteSuggestions(t *testing.T) {
	// 结构不完整的修复/创建建议也要丢弃，不生成卡片：
	// 只验 JSON 能否解码时，空对象 {} 会漏网（全零值也算解码成功）。
	dropRepair := func(args string) *agent.Report {
		t.Helper()
		rep, err := parseDiagnosisArgs(args)
		if err != nil {
			t.Fatalf("结构不完整时报告本身仍应返回: %v", err)
		}
		if rep.SuggestedRepair != nil {
			t.Fatalf("不完整的修复建议应被丢弃，得到 %+v", rep.SuggestedRepair)
		}
		if len(rep.OpenQuestions) == 0 || !strings.Contains(strings.Join(rep.OpenQuestions, ""), "修复建议不完整") {
			t.Fatalf("丢弃修复建议时应在 openQuestions 里说明不完整，得到 %+v", rep.OpenQuestions)
		}
		return rep
	}
	// 空对象：没有 Patches，也没有可转的旧字段。
	dropRepair(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{}}`)
	// Patches 为空数组。
	dropRepair(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","patches":[]}}`)
	// Patch 缺 path。
	dropRepair(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","patches":[{"op":"replace","value":"256Mi"}]}}`)
	// Patch 的 value 为 null。
	dropRepair(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","patches":[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":null}]}}`)
	// Patch 的 op 不是 replace。
	dropRepair(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","patches":[{"op":"add","path":"/spec/replicas","value":3}]}}`)
	// 目标字段缺失但 Patch 合法：namespace / deployment / container
	// 能从任务目标、会话历史或第一个容器兜底，不算不合格，照常保留。
	rep, err := parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"patches":[{"op":"replace","path":"/spec/replicas","value":3}]}}`)
	if err != nil {
		t.Fatalf("目标字段缺失但 Patch 合法时应能解析: %v", err)
	}
	if rep.SuggestedRepair == nil || len(rep.SuggestedRepair.Patches) != 1 {
		t.Fatalf("目标字段可兜底的修复建议应保留，得到 %+v", rep.SuggestedRepair)
	}
	// 只含旧字段但能转成合法 Patch 的，Normalize 之后同样合格。
	rep, err = parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedRepair":{"namespace":"demo","deployment":"demo-api","container":"demo-api","field":"memoryLimit","oldValue":"128Mi","newValue":"256Mi","reason":"内存不足"}}`)
	if err != nil {
		t.Fatalf("旧字段合法时应能解析: %v", err)
	}
	if rep.SuggestedRepair == nil || len(rep.SuggestedRepair.Patches) != 1 || rep.SuggestedRepair.Patches[0].Op != "replace" {
		t.Fatalf("旧字段转成的 Patch 应保留，得到 %+v", rep.SuggestedRepair)
	}
	// 创建建议：缺 name / image 时丢弃，三字段齐全时保留，
	// replicas 为 0 时下游默认按 1 处理，不算不合格。
	dropCreation := func(args string) {
		t.Helper()
		rep, err := parseDiagnosisArgs(args)
		if err != nil {
			t.Fatalf("结构不完整时报告本身仍应返回: %v", err)
		}
		if rep.SuggestedCreation != nil {
			t.Fatalf("不完整的创建建议应被丢弃，得到 %+v", rep.SuggestedCreation)
		}
		if len(rep.OpenQuestions) == 0 || !strings.Contains(strings.Join(rep.OpenQuestions, ""), "创建建议不完整") {
			t.Fatalf("丢弃创建建议时应在 openQuestions 里说明不完整，得到 %+v", rep.OpenQuestions)
		}
	}
	dropCreation(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedCreation":{"namespace":"demo","image":"nginx"}}`)
	dropCreation(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedCreation":{"namespace":"demo","name":"web","replicas":2}}`)
	rep, err = parseDiagnosisArgs(`{"facts":["a"],"cause":"c","confidence":"medium","suggestedCreation":{"namespace":"demo","name":"web","image":"nginx:1.25","replicas":0}}`)
	if err != nil {
		t.Fatalf("三字段齐全时应能解析: %v", err)
	}
	if rep.SuggestedCreation == nil || rep.SuggestedCreation.Name != "web" {
		t.Fatalf("三字段齐全的创建建议应保留，得到 %+v", rep.SuggestedCreation)
	}
}

func TestRealNextCorrectsMalformedDiagnosisOnce(t *testing.T) {
	// 第一次交报告时 facts 是数字（完全不兼容），纠错一次后模型重交正确报告。
	// 纠错只发生一次：总共 2 次模型请求；第 2 次请求的末尾正好是一对
	// assistant（带失败的 tool_calls）+ tool（字段错误）消息，
	// 没有追加任何假 user 消息。
	bad := submitDiagnosisResp("call-1", `{"facts":123,"cause":"节点状态正常","evidenceIds":[],"confidence":"low"}`)
	good := submitDiagnosisResp("call-2", `{"facts":["节点状态正常"],"cause":"节点状态正常","evidenceIds":["ev-1"],"confidence":"low"}`)
	calls := 0
	srv, bodies := scriptedOpenAI(t, []string{bad, good}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("纠错后应返回报告，不应报错: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("纠错后应交最终报告，得到 %+v", step)
	}
	if len(step.Report.Facts) != 1 || step.Report.Facts[0] != "节点状态正常" {
		t.Fatalf("重交的报告内容不对: %+v", step.Report)
	}
	if calls != 2 {
		t.Fatalf("纠错应只多发 1 次请求，总共 2 次，得到 %d 次", calls)
	}
	msgs := requestMessages(t, (*bodies)[1])
	if len(msgs) < 2 {
		t.Fatalf("第 2 次请求应带纠错消息，得到 %+v", msgs)
	}
	// 末尾两条：assistant（带失败的 submit_diagnosis）+ tool（字段错误）。
	assistantMsg, toolMsg := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if assistantMsg.Role != "assistant" || len(assistantMsg.ToolCalls) != 1 || assistantMsg.ToolCalls[0].Function.Name != "submit_diagnosis" {
		t.Fatalf("纠错应追加 assistant + 失败的 submit_diagnosis，得到 %+v", assistantMsg)
	}
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call-1" || !strings.Contains(toolMsg.Content, "facts 应为字符串数组，收到 number") {
		t.Fatalf("纠错的 tool 消息应带字段错误，得到 %+v", toolMsg)
	}
	// 没有追加假 user 消息：纠错前后 user 角色的消息数不变
	//（任务消息 + 证据消息都来自最初的 buildRealMessages，
	// 纠错只追加了 assistant+tool）。
	countUsers := func(msgs []requestMessage) int {
		n := 0
		for _, mm := range msgs {
			if mm.Role == "user" {
				n++
			}
		}
		return n
	}
	if before, after := countUsers(requestMessages(t, (*bodies)[0])), countUsers(msgs); before != after {
		t.Fatalf("纠错不应追加 user 消息，纠错前 %d 条、纠错后 %d 条", before, after)
	}
}

func TestRealNextFallbackKeepsUnverifiedAnalysis(t *testing.T) {
	// 两次都交坏报告且都附带文字：降级报告的 facts 里保留
	// 最后一轮的文字，并标明未经结构化验证。
	bad1 := submitDiagnosisRespWithContent("call-1", "初步看节点没什么问题。", `{"facts":123,"cause":"c","evidenceIds":[],"confidence":"low"}`)
	bad2 := submitDiagnosisRespWithContent("call-2", "节点内存充足，未见异常。", `{"facts":{"a":"b"},"cause":"c","evidenceIds":[],"confidence":"low"}`)
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{bad1, bad2}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("纠错失败后应降级为收敛报告，不应报错: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("应返回低置信度收敛报告，得到 %+v", step)
	}
	if step.Report.Confidence != "low" {
		t.Fatalf("降级报告置信度应为 low，得到 %q", step.Report.Confidence)
	}
	// facts[0] 是格式错误说明，[1] 是附带的未验证分析，其后才是证据摘要。
	if len(step.Report.Facts) < 2 || !strings.Contains(step.Report.Facts[0], "格式有误") {
		t.Fatalf("降级报告 facts 首条应说明格式错误，得到 %+v", step.Report.Facts)
	}
	if !strings.Contains(step.Report.Facts[1], "节点内存充足，未见异常。") || !strings.Contains(step.Report.Facts[1], "未经结构化验证") {
		t.Fatalf("降级报告应保留最后一轮附带的文字并标注未经结构化验证，得到 %+v", step.Report.Facts)
	}
	if calls != 2 {
		t.Fatalf("纠错只发生一次，总共 2 次请求，得到 %d 次", calls)
	}
}

func TestRealNextStringFactsNeedsNoCorrection(t *testing.T) {
	// issue #13 的最小示例直接走 Next：facts 是字符串，解析放宽后
	// 一次就过，不触发纠错。
	resp := submitDiagnosisResp("call-1", `{"facts":"节点状态正常","cause":"节点状态正常，Pod 未受节点影响","evidenceIds":[],"confidence":"low"}`)
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{resp}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("facts 为字符串时应直接解析，不应报错: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("应交最终报告，得到 %+v", step)
	}
	if len(step.Report.Facts) != 1 || step.Report.Facts[0] != "节点状态正常" {
		t.Fatalf("facts 应为 [\"节点状态正常\"]，得到 %+v", step.Report.Facts)
	}
	if calls != 1 {
		t.Fatalf("兼容值不应触发纠错，应只发 1 次请求，得到 %d 次", calls)
	}
}

func TestRealNextFallsBackToLowConfidenceReport(t *testing.T) {
	// 两次都交坏报告：纠错一次仍失败，降级为低置信度收敛报告，
	// 不直接返回错误让整轮作废。
	bad1 := submitDiagnosisResp("call-1", `{"facts":123,"cause":"c","evidenceIds":[],"confidence":"low"}`)
	bad2 := submitDiagnosisResp("call-2", `{"facts":{"a":"b"},"cause":"c","evidenceIds":[],"confidence":"low"}`)
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{bad1, bad2}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("纠错失败后应降级为收敛报告，不应报错: %v", err)
	}
	if !step.IsFinal || step.Report == nil {
		t.Fatalf("应返回低置信度收敛报告，得到 %+v", step)
	}
	if step.Report.Confidence != "low" {
		t.Fatalf("降级报告置信度应为 low，得到 %q", step.Report.Confidence)
	}
	if !strings.Contains(step.Report.Cause, "格式错误") {
		t.Fatalf("降级报告应说明是格式错误，得到 %+v", step.Report)
	}
	found := false
	for _, id := range step.Report.EvidenceIDs {
		if id == "ev-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("降级报告应带上已有证据 ID ev-1，得到 %+v", step.Report.EvidenceIDs)
	}
	if calls != 2 {
		t.Fatalf("纠错只发生一次，总共 2 次请求，得到 %d 次", calls)
	}
}

// ---------------------------------------------------------------------------
// Token 用量统计 + max_tokens
// ---------------------------------------------------------------------------

// toolcallRespWithUsage 是 mockOpenAI "toolcall" 模式的响应，
// 只是多了 usage 对象。
func toolcallRespWithUsage(prompt, completion, total int) string {
	return `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"{\"namespace\":\"demo\",\"pod\":\"demo-api-7d9f6c4b5-xk2v9\"}"}}]}}],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) + `,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(total) + `}}`
}

// submitDiagnosisRespWithUsage 和 submitDiagnosisResp 一样，
// 只是多了 usage 对象。
func submitDiagnosisRespWithUsage(callID, args string, prompt, completion, total int) string {
	return `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"submit_diagnosis","arguments":` + strconv.Quote(args) + `}}]}}],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) + `,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(total) + `}}`
}

func TestRealCapturesUsage(t *testing.T) {
	// 响应里带 usage 时，Next 和 Chat 都要把用量带回调用方，
	// 否则上层没法累计。
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{toolcallRespWithUsage(100, 50, 150)}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if want := (store.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}); step.Usage != want {
		t.Fatalf("Next 应带回用量 %+v，得到 %+v", want, step.Usage)
	}
	cs, err := m.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "demo 有几个 Pod"}})
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if want := (store.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}); cs.Usage != want {
		t.Fatalf("Chat 应带回用量 %+v，得到 %+v", want, cs.Usage)
	}
}

func TestRealUsageMissingIsZero(t *testing.T) {
	// 响应里没有 usage 时用量就是零值，不编造。
	// mockOpenAI 的 toolcall 模式不带 usage。
	calls := 0
	srv := mockOpenAI(t, "toolcall", &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if !step.Usage.IsZero() {
		t.Fatalf("没有 usage 的响应应用量应为零值，得到 %+v", step.Usage)
	}
}

func TestRealUsageTotalDerived(t *testing.T) {
	// usage 里 total_tokens 缺失时按输入+输出补齐，
	// 不把 total 留成 0 误导上层。
	calls := 0
	resp := `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":80,"completion_tokens":20}}`
	srv, _ := scriptedOpenAI(t, []string{resp}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if want := (store.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100}); step.Usage != want {
		t.Fatalf("total 缺失时应按输入+输出补齐为 %+v，得到 %+v", want, step.Usage)
	}
}

func TestRealNextAccumulatesUsageAcrossCorrection(t *testing.T) {
	// 纠错那次额外调用也花了 token：第一次坏报告和第二次好报告的用量
	// 都要算进来，不能因为最后只返回一份报告就把前面的丢了。
	bad := submitDiagnosisRespWithUsage("call-1", `{"facts":123,"cause":"节点状态正常","evidenceIds":[],"confidence":"low"}`, 10, 5, 15)
	good := submitDiagnosisRespWithUsage("call-2", `{"facts":["节点状态正常"],"cause":"节点状态正常","evidenceIds":["ev-1"],"confidence":"low"}`, 20, 10, 30)
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{bad, good}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err != nil {
		t.Fatalf("纠错后应返回报告，不应报错: %v", err)
	}
	if want := (store.Usage{PromptTokens: 30, CompletionTokens: 15, TotalTokens: 45}); step.Usage != want {
		t.Fatalf("两次调用的用量应累加为 %+v，得到 %+v", want, step.Usage)
	}
}

func TestRealMaxTokensOmittedWhenZero(t *testing.T) {
	// MaxTokens=0（默认）时请求体里不应有 max_tokens 字段，
	// 走模型默认，而不是发一个 0 过去让服务端按 0 截断输出。
	calls := 0
	srv, bodies := scriptedOpenAI(t, []string{toolcallRespWithUsage(1, 1, 2)}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	if _, err := m.Next(context.Background(), testInput()); err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if strings.Contains((*bodies)[0], "max_tokens") {
		t.Fatalf("MaxTokens=0 时请求体不应带 max_tokens，得到 %s", (*bodies)[0])
	}
}

func TestRealMaxTokensSentWhenSet(t *testing.T) {
	// MaxTokens 非 0 时请求体里要带上该值，Next 和 Chat 两条路都要走
	// 同一个 callWithRetry，只测 Next 不够。
	calls := 0
	srv, bodies := scriptedOpenAI(t, []string{toolcallRespWithUsage(1, 1, 2)}, &calls)
	defer srv.Close()
	m := NewRealWithConfig(RealConfig{APIKey: "test-key", BaseURL: srv.URL, ModelName: "test-model", HTTPClient: srv.Client(), Timeout: 5 * time.Second, MaxTokens: 2048})
	if _, err := m.Next(context.Background(), testInput()); err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if !strings.Contains((*bodies)[0], `"max_tokens":2048`) {
		t.Fatalf("MaxTokens=2048 时请求体应带 max_tokens:2048，得到 %s", (*bodies)[0])
	}
	if _, err := m.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if !strings.Contains((*bodies)[1], `"max_tokens":2048`) {
		t.Fatalf("Chat 的请求体也应带 max_tokens:2048，得到 %s", (*bodies)[1])
	}
}

func TestRealNextKeepsUsageWhenRetryRequestFails(t *testing.T) {
	// 审查第 1 项：纠错中第二次请求本身失败时，第一轮已经收到的用量
	// 不能跟着错误丢了——Next 出错返回里也要带上已攒的 totalUsage。
	bad := submitDiagnosisRespWithUsage("call-1", `{"facts":123,"cause":"节点状态正常","evidenceIds":[],"confidence":"low"}`, 10, 5, 15)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, bad)
			return
		}
		// 第二次请求直接 400：不进重试、立即失败（500 会进重试退避，测试变慢）。
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err == nil {
		t.Fatalf("第二次请求失败时 Next 应返回错误")
	}
	if calls != 2 {
		t.Fatalf("应只发 2 次请求，得到 %d 次", calls)
	}
	if want := (store.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); step.Usage != want {
		t.Fatalf("出错返回也应带上第一轮的用量 %+v，得到 %+v", want, step.Usage)
	}
}

func TestRealChatKeepsUsageOnParseError(t *testing.T) {
	// Chat 交坏报告解析失败时，出错返回里也要带上那次响应的用量，
	// 否则调用方（runReal）没法记账，第一次烧掉的 token 就丢了。
	bad := submitDiagnosisRespWithUsage("call-1", `{"facts":123,"cause":"容器内存限制过低","evidenceIds":[],"confidence":"low"}`, 40, 20, 60)
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{bad}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	cs, err := m.Chat(context.Background(), []ChatMessage{{Role: "user", Content: "demo-api 为什么重启"}})
	if err == nil {
		t.Fatalf("解析失败时 Chat 应返回错误")
	}
	if _, ok := err.(*DiagnosisParseError); !ok {
		t.Fatalf("应返回 DiagnosisParseError，得到 %T", err)
	}
	if want := (store.Usage{PromptTokens: 40, CompletionTokens: 20, TotalTokens: 60}); cs.Usage != want {
		t.Fatalf("出错返回也应带上本次用量 %+v，得到 %+v", want, cs.Usage)
	}
}

func TestRealNextKeepsUsageOnToolArgsParseError(t *testing.T) {
	// 工具参数解析失败时，出错返回里也要带上那次响应的用量。
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_pod","arguments":"not-json{"}}]}}],"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}}`
	calls := 0
	srv, _ := scriptedOpenAI(t, []string{resp}, &calls)
	defer srv.Close()
	m := newTestReal(t, srv)
	step, err := m.Next(context.Background(), testInput())
	if err == nil {
		t.Fatalf("参数解析失败时 Next 应返回错误")
	}
	if want := (store.Usage{PromptTokens: 30, CompletionTokens: 10, TotalTokens: 40}); step.Usage != want {
		t.Fatalf("出错返回也应带上本次用量 %+v，得到 %+v", want, step.Usage)
	}
}
