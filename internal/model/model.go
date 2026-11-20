// Package model 提供模型适配器。
//
// Slice 1 只有桩模型：按固定顺序走一遍工具调用，最后根据证据给诊断。
// 这样做的好处是演示和测试完全确定，不依赖外部模型 API、
// 不花钱、不断网也能跑。
//
// Slice 2 接入真模型：用 OpenAI 兼容的 chat completions + tools
// 协议，不写死厂商。为什么选这个协议：DeepSeek 和 OpenAI 都支持它，
// 只换 MODEL_BASE_URL 和 MODEL_NAME 就能切厂商，Agent 不用改。
// 模型返回的工具调用先映射成 agent.ToolCall，再由 Agent 的
// ValidateToolCall 校验一次，模型是不可信输入，校验不能只放在模型这边。
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

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
//   5: search_runbooks，关键词取自已看到的证据（优先 OOMKilled，
//      其次 ImagePullBackOff，再次探针失败）
//   6: 交最终报告
//
// 如果是重启恢复，StepIndex 会从已用步数继续，不会从头重走。
// Slice 2 把 Stub 扩展到三类场景（OOM / 镜像拉取 / 探针），
// 仍是确定性剧本，只为让 eval 的三个用例都能在 stub 模式下跑通，
// 不是让 Stub 假装会思考。
type Stub struct{}

// NewStub 返回桩模型。
func NewStub() *Stub { return &Stub{} }

// Next 按 StepIndex 返回下一步。
func (s *Stub) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	ns := in.Namespace
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
		// 关键词按已看到的证据选：OOM 优先，其次镜像拉取，再次探针。
		kw := keywordFromHistory(in)
		return call("search_runbooks", map[string]string{"query": kw}), nil
	default:
		return agent.ModelStep{IsFinal: true, Report: s.buildReport(in)}, nil
	}
}

// keywordFromHistory 从已收集的证据里选 search_runbooks 的关键词。
func keywordFromHistory(in agent.ModelInput) string {
	joined := ""
	for _, h := range in.History {
		joined += h.Summary + " " + h.ResultJSON + " "
	}
	switch {
	case strings.Contains(joined, "OOMKilled") || strings.Contains(joined, "out of memory"):
		return "OOMKilled"
	case strings.Contains(joined, "ImagePullBackOff") || strings.Contains(joined, "Failed to pull image") || strings.Contains(joined, "ErrImagePull"):
		return "ImagePullBackOff"
	case strings.Contains(joined, "Liveness probe failed") || strings.Contains(joined, "Unhealthy") || strings.Contains(joined, "探针"):
		return "Liveness probe failed"
	default:
		return in.Question
	}
}

func discoverPodName(in agent.ModelInput) string {
	for _, h := range in.History {
		if h.ToolName == "list_workload_pods" && h.ResultJSON != "" {
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
// 并且只有看到明确信号才下结论，否则诚实地说证据不足。
func (s *Stub) buildReport(in agent.ModelInput) *agent.Report {
	ids := make([]string, 0, len(in.Evidence))
	for id := range in.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	joined := ""
	primary := "" // 一手证据：Pod / Events / 日志 / Deployment，不含检索结果
	var facts []string
	for _, h := range in.History {
		if h.Summary != "" {
			facts = append(facts, h.Summary)
		}
		joined += h.Summary + " " + h.ResultJSON + " "
		if h.ToolName != "search_runbooks" {
			primary += h.Summary + " " + h.ResultJSON + " "
		}
	}
	if len(facts) == 0 {
		facts = []string{"未收集到有效证据"}
	}
	// 先看一手证据，再看检索结果：search_runbooks 的 FTS 是 OR 查询，
	// 可能带回一两条相关但不是本案的手册（例如查探针时带回镜像手册，
	// 因为两篇都含 failed），检索片段不能压过 Pod / Events / 日志
	// 这些一手证据，否则探针案会被误判成镜像案。
	seenOOM := strings.Contains(primary, "OOMKilled")
	seenImagePull := strings.Contains(primary, "ImagePullBackOff") || strings.Contains(primary, "Failed to pull image") || strings.Contains(primary, "ErrImagePull")
	seenProbe := strings.Contains(primary, "Liveness probe failed") || strings.Contains(primary, "Unhealthy")
	if !seenOOM && !seenImagePull && !seenProbe {
		seenOOM = strings.Contains(joined, "OOMKilled")
		seenImagePull = strings.Contains(joined, "ImagePullBackOff") || strings.Contains(joined, "Failed to pull image")
		seenProbe = strings.Contains(joined, "Liveness probe failed") || strings.Contains(joined, "Unhealthy")
	}
	switch {
	case seenOOM:
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
	case seenImagePull:
		return &agent.Report{
			Facts:       facts,
			Cause:       "镜像拉取失败，Pod 卡在 ImagePullBackOff，容器一直没起来",
			EvidenceIDs: ids,
			Recommendations: []string{
				"核对 Deployment 的 image 字段和 tag 是否写错",
				"确认镜像仓库是否可达，imagePullSecrets 是否缺失",
				"修正镜像后重新部署并观察 Pod 是否进入 Running",
			},
			OpenQuestions: []string{"镜像仓库的访问凭证是否正确，仍需进一步确认"},
			Confidence:    "high",
		}
	case seenProbe:
		return &agent.Report{
			Facts:       facts,
			Cause:       "探针失败，Liveness 探针失败导致容器被重启",
			EvidenceIDs: ids,
			Recommendations: []string{
				"检查探针的路径、端口和超时是否合理",
				"确认应用启动耗时是否超过 initialDelaySeconds",
				"查看日志里是否有启动失败，再调整探针参数",
			},
			OpenQuestions: []string{"应用启动慢是代码问题还是探针配得太严，仍需进一步确认"},
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

// ---------------------------------------------------------------------------
// 真模型：OpenAI 兼容协议
// ---------------------------------------------------------------------------

// RealConfig 是真模型的配置，全部来自环境变量，不写进代码和仓库。
type RealConfig struct {
	APIKey     string
	BaseURL    string // 例如 https://api.deepseek.com/v1，默认 DeepSeek
	ModelName  string // 例如 deepseek-chat，默认 deepseek-chat
	HTTPClient *http.Client
	MaxRetries int           // 网络 / 429 / 5xx 的重试次数，默认 3
	Timeout    time.Duration // 每次请求的超时，默认 30 秒
}

func (c RealConfig) withDefaults() RealConfig {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.deepseek.com/v1"
	}
	if c.ModelName == "" {
		c.ModelName = "deepseek-chat"
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c
}

// Real 是真模型适配器。
// 它只负责协议：把 Agent 的上下文拼成 chat completions 请求，
// 把模型返回的 tool_calls 映射成 agent.ToolCall，
// 把 submit_diagnosis 映射成最终报告。
// 工具名、namespace、参数的合法性不在这里最终决定，
// Agent 的 ValidateToolCall 还会再校验一次。
type Real struct {
	cfg RealConfig
}

// NewReal 返回真模型（兼容 Slice 1 的调用方式，默认 DeepSeek）。
func NewReal(apiKey string) *Real {
	return NewRealWithConfig(RealConfig{APIKey: apiKey})
}

// NewRealWithConfig 用显式配置创建真模型，测试时可注入 httptest 的地址。
func NewRealWithConfig(cfg RealConfig) *Real {
	return &Real{cfg: cfg.withDefaults()}
}

// NewRealFromEnv 从环境变量创建真模型。
// MODEL_API_KEY 缺失时明确报错，启动就失败，不让服务带着一个
// 调不通的模型跑起来再在运行时才暴露问题。
func NewRealFromEnv() (*Real, error) {
	key := strings.TrimSpace(os.Getenv("MODEL_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("MODEL_MODE=real 但未设置 MODEL_API_KEY，请从环境变量传入，不要写进代码或仓库")
	}
	return NewRealWithConfig(RealConfig{
		APIKey:    key,
		BaseURL:   os.Getenv("MODEL_BASE_URL"),
		ModelName: os.Getenv("MODEL_NAME"),
	}), nil
}

// chatMessage / chat 请求与响应，只按 OpenAI 兼容协议的最小字段定义。
type chatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatRequest struct {
	Model       string         `json:"model"`
	Messages    []chatMessage  `json:"messages"`
	Tools       []chatToolDef  `json:"tools,omitempty"`
	ToolChoice  string         `json:"tool_choice,omitempty"`
	Temperature float64        `json:"temperature"`
}

type chatToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// toolDefs 是发给模型的工具定义。
// 6 个诊断工具和一个 submit_diagnosis：模型要交最终报告时
// 必须调 submit_diagnosis，而不是直接写一段自由文本，
// 这样报告字段才是结构化的，Agent 才能校验证据引用。
func toolDefs() []chatToolDef {
	strParam := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	def := func(name, desc string, props map[string]any, required []string) chatToolDef {
		var d chatToolDef
		d.Type = "function"
		d.Function.Name = name
		d.Function.Description = desc
		d.Function.Parameters = map[string]any{"type": "object", "properties": props, "required": required}
		return d
	}
	ns := map[string]any{"namespace": strParam("命名空间，必须和任务一致")}
	return []chatToolDef{
		def("list_workload_pods", "列出指定 Deployment 下的 Pod", merge(ns, map[string]any{"deployment": strParam("Deployment 名称")}), []string{"namespace", "deployment"}),
		def("get_pod", "查询单个 Pod 的状态、重启次数和上次终止原因", merge(ns, map[string]any{"pod": strParam("Pod 名称")}), []string{"namespace", "pod"}),
		def("get_events", "查询和目标相关的 Events", merge(ns, map[string]any{"name": strParam("Pod 或 Deployment 名称")}), []string{"namespace", "name"}),
		def("get_pod_logs", "读取指定容器的日志", merge(ns, map[string]any{"pod": strParam("Pod 名称"), "container": strParam("容器名"), "previous": map[string]any{"type": "boolean", "description": "是否读上一次容器的日志"}}), []string{"namespace", "pod"}),
		def("get_deployment", "查询 Deployment 的副本数和容器资源配置", merge(ns, map[string]any{"deployment": strParam("Deployment 名称")}), []string{"namespace", "deployment"}),
		def("search_runbooks", "检索排障手册", map[string]any{"query": strParam("检索关键词，例如 OOMKilled、ImagePullBackOff、Liveness probe failed")}, []string{"query"}),
		def("submit_diagnosis", "提交最终诊断报告，证据 ID 只能引用已返回的工具结果里的 ID", map[string]any{
			"facts":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "观测事实，每条来自工具结果"},
			"cause":           strParam("原因判断"),
			"evidenceIds":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "引用的证据 ID，只能用已返回的 ID"},
			"recommendations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"openQuestions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"confidence":      map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
		}, []string{"facts", "cause", "evidenceIds", "confidence"}),
	}
}

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

const realSystemPrompt = `你是 Kubernetes 故障诊断助手。任务是根据只读工具的结果诊断 Pod 或 Deployment 的故障。

规则：
1. 只能调用提供的工具，工具参数里的 namespace 必须和任务一致，不要查其他 namespace，不要查 kube-system。
2. 一次只调一个工具，先查 Pod 状态、事件和日志，再查 Deployment 和手册，不要凭空猜测。
3. 证据不足时不要硬编结论，调用 submit_diagnosis 时把 confidence 写成 low，并写清还需要补查什么。
4. 提交诊断必须调用 submit_diagnosis，evidenceIds 只能引用工具结果里已经出现过的 ID。`

// Next 问一次真模型，返回一步工具调用或最终报告。
func (r *Real) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	if strings.TrimSpace(r.cfg.APIKey) == "" {
		return agent.ModelStep{}, fmt.Errorf("真模型缺少 API Key，请设置 MODEL_API_KEY")
	}
	msgs := buildRealMessages(in)
	resp, err := r.callWithRetry(ctx, msgs)
	if err != nil {
		return agent.ModelStep{}, err
	}
	if len(resp.Choices) == 0 {
		return agent.ModelStep{}, fmt.Errorf("模型返回为空，没有 choices")
	}
	msg := resp.Choices[0].Message
	// 优先处理工具调用：一次只取第一个，剩下的下一轮再让模型决定，
	// 避免一次返回多个调用、Agent 难以按顺序持久化的问题。
	if len(msg.ToolCalls) > 0 {
		tc := msg.ToolCalls[0]
		name := tc.Function.Name
		if name == "submit_diagnosis" {
			rep, err := parseDiagnosisArgs(tc.Function.Arguments)
			if err != nil {
				return agent.ModelStep{}, fmt.Errorf("模型提交的诊断报告解析失败: %w", err)
			}
			return agent.ModelStep{IsFinal: true, Report: rep}, nil
		}
		args, err := parseToolArgs(tc.Function.Arguments)
		if err != nil {
			return agent.ModelStep{}, fmt.Errorf("模型返回的工具参数解析失败: %w", err)
		}
		// 模型漏填 namespace 时补上任务的 namespace，缺了它 Agent 会拒绝执行。
		if _, ok := args["namespace"]; !ok || args["namespace"] == "" {
			if name != "search_runbooks" {
				args["namespace"] = in.Namespace
			}
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call-%d", in.StepIndex+1)
		}
		// 注意：这里不做最终校验，未知工具名会原样交给 Agent，
		// Agent 的 ValidateToolCall 会拒绝它并记一步错误，循环继续。
		return agent.ModelStep{ToolCall: &agent.ToolCall{ID: id, Name: name, Args: args}}, nil
	}
	// 没有工具调用时，把文本当作最终结论处理。
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return agent.ModelStep{}, fmt.Errorf("模型既没有调工具也没有返回文本")
	}
	if rep, err := tryParseReportJSON(content); err == nil {
		return agent.ModelStep{IsFinal: true, Report: rep}, nil
	}
	ids := make([]string, 0, len(in.Evidence))
	for id := range in.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return agent.ModelStep{IsFinal: true, Report: &agent.Report{
		Facts: []string{content}, Cause: content, EvidenceIDs: ids,
		Recommendations: []string{"根据报告进一步核对证据后再决定是否修复"},
		OpenQuestions:   []string{"模型未用结构化方式提交诊断，结论需人工复核"},
		Confidence:      "medium",
	}}, nil
}

// buildRealMessages 把任务和历史拼成发给模型的消息。
// 为什么要截断：日志全量已经很大，历史全塞进去会撑爆上下文，
// 还会烧很多 token。这里只传最近 6 步的摘要，每条最多 500 字，
// 全量证据仍在 SQLite 里，前端时间线能看到，模型不需要全量。
func buildRealMessages(in agent.ModelInput) []chatMessage {
	msgs := []chatMessage{
		{Role: "system", Content: realSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("任务：namespace=%s，类型=%s，名称=%s，问题：%s\n已走 %d 步，请继续诊断。", in.Namespace, in.Kind, in.Name, in.Question, in.StepIndex)},
	}
	// 历史只取最近 6 步，避免上下文无限增长。
	hist := in.History
	if len(hist) > 6 {
		hist = hist[len(hist)-6:]
	}
	for _, h := range hist {
		summary := truncateRunes(h.Summary, 500)
		if h.Error != "" {
			summary += "（这一步出错：" + truncateRunes(h.Error, 200) + "）"
		}
		msgs = append(msgs, chatMessage{Role: "assistant", Content: fmt.Sprintf("已调用 %s：%s", h.ToolName, summary)})
	}
	if len(in.Evidence) > 0 {
		ids := make([]string, 0, len(in.Evidence))
		for id := range in.Evidence {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var b strings.Builder
		b.WriteString("已有证据 ID（提交诊断时只能引用这些）：\n")
		for _, id := range ids {
			b.WriteString("- " + id + ": " + truncateRunes(in.Evidence[id], 200) + "\n")
		}
		msgs = append(msgs, chatMessage{Role: "user", Content: b.String()})
	}
	return msgs
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（已截断）"
}

// callWithRetry 调一次 chat completions，429 / 5xx / 网络错误才重试，
// 400 这类请求错误不重试。退避是指数退避，最多 3 次。
func (r *Real) callWithRetry(ctx context.Context, msgs []chatMessage) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model: r.cfg.ModelName, Messages: msgs, Tools: toolDefs(),
		ToolChoice: "auto", Temperature: 0,
	})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= r.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(500*(1<<(attempt-1))) * time.Millisecond):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
		resp, err := r.cfg.HTTPClient.Do(req)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("调用模型 API 失败: %w", err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("模型 API 返回 %d：%s", resp.StatusCode, truncateRunes(string(data), 300))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("模型 API 返回 %d：%s", resp.StatusCode, truncateRunes(string(data), 300))
		}
		var out chatResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("解析模型返回失败: %w", err)
		}
		if out.Error != nil {
			return nil, fmt.Errorf("模型 API 报错：%s", out.Error.Message)
		}
		return &out, nil
	}
	return nil, fmt.Errorf("调用模型 API 多次重试后仍失败：%v", lastErr)
}

// parseToolArgs 把工具参数的 JSON 字符串转成 map[string]string。
// 模型可能把 previous 写成布尔值、把数字写成数字，这里都转成字符串，
// Agent 侧按字符串解析。
func parseToolArgs(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]string{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case float64:
			out[k] = fmt.Sprintf("%v", t)
		case nil:
			out[k] = ""
		default:
			b, _ := json.Marshal(t)
			out[k] = string(b)
		}
	}
	return out, nil
}

// parseDiagnosisArgs 解析 submit_diagnosis 的参数。
func parseDiagnosisArgs(raw string) (*agent.Report, error) {
	var v struct {
		Facts           []string `json:"facts"`
		Cause           string   `json:"cause"`
		EvidenceIDs     []string `json:"evidenceIds"`
		Recommendations []string `json:"recommendations"`
		OpenQuestions   []string `json:"openQuestions"`
		Confidence      string   `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err != nil {
		return nil, err
	}
	if strings.TrimSpace(v.Cause) == "" {
		return nil, fmt.Errorf("诊断报告缺少 cause")
	}
	if v.Confidence == "" {
		v.Confidence = "medium"
	}
	return &agent.Report{
		Facts: v.Facts, Cause: v.Cause, EvidenceIDs: v.EvidenceIDs,
		Recommendations: v.Recommendations, OpenQuestions: v.OpenQuestions,
		Confidence: v.Confidence,
	}, nil
}

func tryParseReportJSON(content string) (*agent.Report, error) {
	// 模型可能在文本前后加解释，先找第一个 { 到最后一个 }。
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("不是 JSON")
	}
	return parseDiagnosisArgs(content[start : end+1])
}
