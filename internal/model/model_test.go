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
