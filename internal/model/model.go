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
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/mcpclient"
	"kubecause/internal/store"
)

// Stub 是确定性桩模型。
//
// 它按 StepIndex（轮次）决定下一步：
//
//	0: list_workload_pods（Deployment 任务）或 get_pod（Pod 任务）
//	1: get_pod
//	2: get_events
//	3: get_pod_logs（当前日志）
//	4: get_deployment
//	5: search_runbooks，关键词取自已看到的证据（优先 OOMKilled，
//	   其次 ImagePullBackOff，再次探针失败）
//	6: 交最终报告
//
// 如果是重启恢复，StepIndex 会从已用轮次继续，不会从头重走。
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
		// Pod 任务的目标 Deployment 从已查到的 Pod 归属里读，
		// 读不到才用 fake 固定数据的默认值。
		deployName = discoverDeploymentName(in)
	}
	call := func(name string, args map[string]string) agent.ModelStep {
		if args == nil {
			args = map[string]string{}
		}
		args["namespace"] = ns
		return agent.ModelStep{
			ToolCalls: []*agent.ToolCall{{ID: fmt.Sprintf("call-%d", in.StepIndex+1), Name: name, Args: args}},
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
		// 容器名不要写死：从已查到的 Deployment 里读第一个容器，
		// 读不到才用 fake 固定数据的默认值（见 discoverContainerName）。
		return call("get_pod_logs", map[string]string{"pod": podName, "container": discoverContainerName(in), "previous": "false"}), nil
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
	// 先按结构化结果解析：list_workload_pods 的 ResultJSON 里有 pods 行列表，
	// 每行是带 name 的对象，取第一行的名字，不认具体名字，
	// 这样换一个 Deployment 也能工作。
	for _, h := range in.History {
		if h.ToolName == "list_workload_pods" && h.ResultJSON != "" {
			var v struct {
				Pods []struct {
					Name string `json:"name"`
				} `json:"pods"`
			}
			if err := json.Unmarshal([]byte(h.ResultJSON), &v); err == nil && len(v.Pods) > 0 && v.Pods[0].Name != "" {
				return v.Pods[0].Name
			}
		}
	}
	// 兜底：fake 固定数据的 Pod 名。Stub 只在 fake/stub 回归里用，
	// 真集群上 Pod 名一定来自上一步 list 的结果，不会走到这里。
	return "demo-api-7d9f6c4b5-xk2v9"
}

// discoverDeploymentName 从已查到的 Pod 结果里读归属 Deployment。
// deployNameForReport 返回报告里修复建议的目标 Deployment：
// Deployment 任务就是任务名，Pod 任务从 Pod 归属里读。
func deployNameForReport(in agent.ModelInput) string {
	if in.Kind == "Deployment" {
		return in.Name
	}
	return discoverDeploymentName(in)
}

func discoverDeploymentName(in agent.ModelInput) string {
	for _, h := range in.History {
		if h.ToolName == "get_pod" && h.ResultJSON != "" {
			var v struct {
				Deployment string `json:"deployment"`
			}
			if err := json.Unmarshal([]byte(h.ResultJSON), &v); err == nil && v.Deployment != "" {
				return v.Deployment
			}
		}
	}
	return "demo-api"
}

func discoverContainerName(in agent.ModelInput) string {
	for _, h := range in.History {
		if h.ToolName == "get_deployment" && h.ResultJSON != "" {
			var v struct {
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
			}
			if err := json.Unmarshal([]byte(h.ResultJSON), &v); err == nil && len(v.Containers) > 0 {
				return v.Containers[0].Name
			}
		}
	}
	return "demo-api"
}

// discoverImage 从已查到的 Deployment 结果里读第一个容器的镜像。
func discoverImage(in agent.ModelInput) string {
	for _, h := range in.History {
		if h.ToolName == "get_deployment" && h.ResultJSON != "" {
			var v struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			}
			if err := json.Unmarshal([]byte(h.ResultJSON), &v); err == nil && len(v.Containers) > 0 {
				return v.Containers[0].Image
			}
		}
	}
	return ""
}

// discoverMemoryLimit 从已查到的 Deployment 结果里读第一个容器的内存 limit。
func discoverMemoryLimit(in agent.ModelInput) string {
	for _, h := range in.History {
		if h.ToolName == "get_deployment" && h.ResultJSON != "" {
			var v struct {
				Containers []struct {
					MemoryLimit string `json:"memoryLimit"`
				} `json:"containers"`
			}
			if err := json.Unmarshal([]byte(h.ResultJSON), &v); err == nil && len(v.Containers) > 0 {
				return v.Containers[0].MemoryLimit
			}
		}
	}
	return ""
}

// doubleMemoryValue 把 "128Mi" 翻倍成 "256Mi"，"2Gi" 翻倍封顶 "4Gi"，
// 供 Stub 的修复建议用；解析失败返回 ""，表示给不出具体新值。
func doubleMemoryValue(old string) string {
	if len(old) < 3 {
		return ""
	}
	suffix := old[len(old)-2:]
	num := 0
	for _, c := range old[:len(old)-2] {
		if c < '0' || c > '9' {
			return ""
		}
		num = num*10 + int(c-'0')
	}
	next := num * 2
	if suffix == "Gi" {
		if next > 4 {
			next = 4
		}
		return fmt.Sprintf("%dGi", next)
	}
	if suffix != "Mi" {
		return ""
	}
	if next > 4096 {
		next = 4096
	}
	return fmt.Sprintf("%dMi", next)
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
		// 修复建议从证据里算出来，不写死目标和数值：
		// 容器和旧值来自已查到的 Deployment，新值是旧值翻倍（封顶 4Gi）。
		// 算不出来时只给文字建议、不给结构化建议，Worker 不会生成提案。
		container := discoverContainerName(in)
		oldLimit := discoverMemoryLimit(in)
		newLimit := doubleMemoryValue(oldLimit)
		recs := []string{
			"检查应用是否存在内存泄漏，确认调高后的 limit 是否足够",
			"观察调高后的重启次数是否停止增长",
		}
		var sr *agent.SuggestedRepair
		if oldLimit != "" && newLimit != "" {
			recs = append([]string{fmt.Sprintf("将 %s 容器的内存 limit 从 %s 调高到 %s（需人工审批后执行）", container, oldLimit, newLimit)}, recs...)
			sr = &agent.SuggestedRepair{
				Namespace: in.Namespace, Deployment: deployNameForReport(in),
				Container: container, Field: "memoryLimit",
				OldValue: oldLimit, NewValue: newLimit,
				Reason: "容器内存限制过低，触发 OOMKilled 导致反复重启",
			}
			// 旧字段转成通用 Patch 格式再返回，和真模型路径一致。
			sr.Normalize()
		} else {
			recs = append([]string{"将容器的内存 limit 适当调高（需人工审批后执行）"}, recs...)
		}
		return &agent.Report{
			Facts:           facts,
			Cause:           "容器内存限制过低，触发 OOMKilled 导致反复重启",
			EvidenceIDs:     ids,
			Recommendations: recs,
			OpenQuestions:   []string{"应用本身是否存在内存泄漏，仍需进一步确认"},
			Confidence:      "high",
			SuggestedRepair: sr,
		}
	case seenImagePull:
		// 镜像拉取失败也可以给一张结构化建议卡，和 OOM 的内存卡
		// 走同一套通用 Patch：改镜像的 path 是
		// /spec/template/spec/containers/0/image。
		// 这是 Stub 测试路径的确定性规则：fake 场景里坏镜像固定是
		// demo-api:bad-tag，好镜像是 demo-api:latest（OOM 底子的镜像），
		// 所以只能在这个场景里给；真模型路径不走这里，
		// 新镜像由模型按证据和用户回答决定，后端只校验格式。
		container := discoverContainerName(in)
		badImage := discoverImage(in)
		var sr *agent.SuggestedRepair
		recs := []string{
			"核对 Deployment 的 image 字段和 tag 是否写错",
			"确认镜像仓库是否可达，imagePullSecrets 是否缺失",
			"修正镜像后重新部署并观察 Pod 是否进入 Running",
		}
		if badImage == "demo-api:bad-tag" {
			recs = append([]string{fmt.Sprintf("将 %s 容器的镜像从 %s 换成 demo-api:latest（需人工审批后执行）", container, badImage)}, recs...)
			sr = &agent.SuggestedRepair{
				Namespace: in.Namespace, Deployment: deployNameForReport(in),
				Container: container, Field: "image",
				OldValue: badImage, NewValue: "demo-api:latest",
				Reason: "镜像 tag 不存在，拉取失败导致 Pod 卡在 ImagePullBackOff",
			}
			sr.Normalize()
		}
		return &agent.Report{
			Facts:           facts,
			Cause:           "镜像拉取失败，Pod 卡在 ImagePullBackOff，容器一直没起来",
			EvidenceIDs:     ids,
			Recommendations: recs,
			OpenQuestions:   []string{"镜像仓库的访问凭证是否正确，仍需进一步确认"},
			Confidence:      "high",
			SuggestedRepair: sr,
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
// Timeout / Temperature 由配置中心（internal/config）填，
// POST /api/config 热更新时调 Real.ApplyModelSettings 改，不用重启。
type RealConfig struct {
	APIKey      string
	BaseURL     string // 例如 https://api.deepseek.com/v1，默认 DeepSeek
	ModelName   string // 例如 deepseek-chat，默认 deepseek-chat
	HTTPClient  *http.Client
	MaxRetries  int           // 网络 / 429 / 5xx 的重试次数，默认 3
	Timeout     time.Duration // 每次请求的超时，默认 30 秒
	Temperature float64       // 模型温度，0-1，诊断场景默认 0.2
	// Debug 打开时，报告解析失败的字段与片段会打到服务端调试日志
	//（KUBE_DEBUG_MODEL=1，默认关闭）。
	Debug bool
	// MaxTokens 是每次请求的最大输出 token（token 用量统计的配套配置）：
	// 0 表示不传该字段，走模型默认；非 0 时按配置发 max_tokens。
	// 宽松默认 4096：诊断报告一般只有几百 token，基本碰不到上限，
	// 只是防单次输出失控。累计预算不在这里做——等真实用量数据
	// 出来再定，不凭感觉拍一个数。
	MaxTokens int
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
// mu 保护 cfg：配置中心热更新超时和温度时，
// 正在进行的请求不能读到改了一半的配置。
type Real struct {
	mu  sync.RWMutex
	cfg RealConfig
	// extraTools 是从 MCP 发现的额外工具定义（节点工具）：
	// 启动时 main.go 从 MCP Client 的 tools/list 结果转过来。
	// 没有 MCP Client 时为空，工具表就是原来的 12 个。
	extraTools []chatToolDef
}

// getCfg 返回当前配置的一份拷贝。
func (r *Real) getCfg() RealConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// SetExtraTools 把 MCP 发现的工具定义并进模型的工具表：
// 调用方（main.go）在 MCP Client 接通后调一次，
// 之后 Next / Chat 发给大模型的工具表就是原来的 12 个 + 这些。
// 传空或不调时工具表不变——没有 MCP Client 的部署不受影响。
func (r *Real) SetExtraTools(defs []mcpclient.ToolDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.extraTools = nil
	for _, d := range defs {
		params := d.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		var cd chatToolDef
		cd.Type = "function"
		cd.Function.Name = d.Name
		cd.Function.Description = d.Description
		cd.Function.Parameters = params
		r.extraTools = append(r.extraTools, cd)
	}
}

// SetDebug 打开/关闭真模型的调试日志：
// 报告解析失败的字段与片段只在打开时记（KUBE_DEBUG_MODEL=1，
// main.go 里和 chatSvc.DebugModel 一起开），默认关闭。
func (r *Real) SetDebug(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Debug = v
}

// toolDefs 返回本次问模型时用的工具表：
// 原来的 12 个 + MCP 发现的节点工具（有的话）。
// 节点工具和 k8s 工具放一起，模型自己决定调不调，
// 后端不替模型挑——该不该查节点由模型按证据判断。
func (r *Real) toolDefs() []chatToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.extraTools) == 0 {
		return toolDefs()
	}
	base := toolDefs()
	out := make([]chatToolDef, 0, len(base)+len(r.extraTools))
	out = append(out, base...)
	out = append(out, r.extraTools...)
	return out
}

// ApplyModelSettings 热更新模型的超时、温度和单次最大输出 token
//（配置中心 POST 后调），不用重启；有任何一个值非法时都不改，保持旧值。
func (r *Real) ApplyModelSettings(timeout time.Duration, temperature float64, maxTokens int) {
	if timeout <= 0 || temperature < 0 || temperature > 1 || maxTokens < 0 || maxTokens > 32768 {
		return
	}
	r.mu.Lock()
	r.cfg.Timeout = timeout
	r.cfg.Temperature = temperature
	r.cfg.MaxTokens = maxTokens
	r.mu.Unlock()
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
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
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
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatToolDef `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature"`
	// MaxTokens 是单次最大输出 token，0 时不发这个字段（omitempty），
	// 走模型默认；非 0 时按配置中心 model.maxTokens 发。
	MaxTokens int `json:"max_tokens,omitempty"`
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
	// Usage 是这次调用的 token 用量（token 用量统计）：
	// 服务端没返回时全为零，调用方只累加非零值，绝不编造。
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// usageOf 从一次 chat completions 响应里取 token 用量：
// 没有 usage 字段时返回零值，不编造；total 缺失时按输入+输出补齐。
func usageOf(resp *chatResponse) store.Usage {
	if resp == nil {
		return store.Usage{}
	}
	return store.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}.Normalized()
}

// toolsWithNamespace 是内置工具里顶层参数声明了 namespace 的集合，
// 从 toolDefs() 的 schema 推导出来，不手写工具名：
// 只有这些工具才会在模型漏填时被补上任务的 namespace。
// MCP 发现的外部工具不在 toolDefs() 里——它们的 InputSchema 由
// Server 自己定，模型没给的参数不能硬塞，SDK 会拒绝 schema 之外的
// 字段（issue #11：正式诊断里调 node_* 工具被多补的 namespace 挡掉）。
// search_runbooks / list_namespaces / get_cluster_overview 的 schema
// 里本来就没有 namespace，也不补。
var toolsWithNamespace = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range toolDefs() {
		props, ok := d.Function.Parameters["properties"].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := props["namespace"]; ok {
			m[d.Function.Name] = true
		}
	}
	return m
}()

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
		def("list_workload_pods", "列出指定 Deployment 下的 Pod，每条带 Pod 名、命名空间、状态、就绪情况、重启次数、归属 Deployment 和终止/等待原因；只返回这个 Deployment 下的 Pod，整个命名空间的全部 Pod 由 list_pods 返回", merge(ns, map[string]any{"deployment": strParam("Deployment 名称")}), []string{"namespace", "deployment"}),
		def("get_pod", "查询单个 Pod，返回它的状态（phase）、重启次数、上次终止原因、容器等待原因和归属 Deployment；只返回这一个 Pod 的信息", merge(ns, map[string]any{"pod": strParam("Pod 名称")}), []string{"namespace", "pod"}),
		def("get_events", "查询和目标 Pod 或 Deployment 相关的 Events，返回最近 20 条，每条有原因、消息和关联对象；只返回事件，不返回 Pod 当前状态", merge(ns, map[string]any{"name": strParam("Pod 或 Deployment 名称")}), []string{"namespace", "name"}),
		def("get_pod_logs", "读取指定容器的一段日志，返回截断后的日志文本（最多 100 行 / 64KB，超了会标注已截断）；previous 为 true 时读上一次容器的日志，否则读当前容器的日志", merge(ns, map[string]any{"pod": strParam("Pod 名称"), "container": strParam("容器名"), "previous": map[string]any{"type": "boolean", "description": "是否读上一次容器的日志"}}), []string{"namespace", "pod"}),
		def("get_deployment", "查询单个 Deployment，返回副本数和容器列表，每个容器带镜像、内存 requests 和 limits；不返回它下面每个 Pod 的状态", merge(ns, map[string]any{"deployment": strParam("Deployment 名称")}), []string{"namespace", "deployment"}),
		def("search_runbooks", "按关键词检索排障手册，返回相关片段，每条带标题、来源文件和关键句；只返回手册内容，不返回集群现场数据", map[string]any{"query": strParam("检索关键词，例如 OOMKilled、ImagePullBackOff、Liveness probe failed")}, []string{"query"}),
		// 发现工具（只读）：对话模式里先看全貌、找问题，再用定向工具深挖。
		// namespace 为空表示查全部命名空间；系统命名空间的结果也会返回，
		// 行里带 isSystem 标记（Slice 5 起读放开、写挡住）。
		// 描述里只写每个工具返回什么、覆盖多大范围，不写“不要调 xxx”：
		// 该调哪个由模型按返回内容自己判断，写死禁止规则会替模型做决策。
		def("list_namespaces", "列出集群的全部命名空间，返回名字列表和总数（含系统命名空间，另有 systemNamespaces 子集）；只返回名字，不返回各命名空间下的 Pod 状态", map[string]any{}, []string{}),
		def("list_deployments", "列出 Deployment 列表，每条带命名空间、副本数、就绪副本数、已更新和可用副本数；namespace 为空时返回全部命名空间的，指定 namespace 时只返回那个命名空间的；不返回 Pod 详情", map[string]any{"namespace": strParam("命名空间，为空表示全部命名空间，指定时只查那个命名空间")}, []string{}),
		def("list_problem_pods", "列出有问题的 Pod（非 Running、未 Ready、重启过、有等待原因），每条带 Pod 名、命名空间、状态、就绪情况、重启次数、归属 Deployment 和终止/等待原因；namespace 为空时一次返回全部命名空间的有问题 Pod，指定 namespace 时只返回那个命名空间的", map[string]any{"namespace": strParam("命名空间，为空表示全部命名空间，指定时只查那个命名空间")}, []string{}),
		def("list_pods", "列出某命名空间下全部 Pod（不限于有问题的），每条带 Pod 名、命名空间、状态、就绪情况、重启次数、归属 Deployment 和终止/等待原因；namespace 为空时一次返回全部命名空间的全部 Pod，指定 namespace 时只返回那个命名空间的", map[string]any{"namespace": strParam("命名空间，为空表示全部命名空间，指定时只查那个命名空间")}, []string{}),
		def("get_cluster_overview", "一次返回全集群的汇总：节点数、命名空间数（含系统命名空间数）、Deployment 数、Pod 总数、有问题 Pod 数（含系统命名空间的有问题 Pod 数）、未完全就绪的 Deployment 数；只返回汇总数字，不返回具体的 Pod 或 Deployment 列表", map[string]any{}, []string{}),
		def("submit_diagnosis", "提交最终诊断报告，证据 ID 只能引用已返回的工具结果里的 ID", map[string]any{
			"facts":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "观测事实，每条来自工具结果"},
			"cause":           strParam("原因判断"),
			"evidenceIds":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "引用的证据 ID，只能用已返回的 ID"},
			"recommendations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"openQuestions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"confidence":      map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
			// 修复建议是可选的：只有在证据或用户明确要求指向一次具体变更时才给，
			// 用 patches 列表描述要改的字段，每个 Patch 是 op/path/value：
			// op 只支持 replace；path 是白名单里的字段，例如改镜像
			// /spec/template/spec/containers/0/image、改内存 limit
			// /spec/template/spec/containers/0/resources/limits/memory、
			// 改副本数 /spec/replicas；value 是新值，例如 "nginx:1.25"、
			// "256Mi"、3。用户明确说了新值（例如“换成 nginx:1.25”、
			// “增加 128Mi”、“调到 256Mi”）时按用户说的算：
			// 先查当前值，再按用户说的增量或目标值算，不要默认翻倍。
			// 没有用户明确要求时也按证据算一个合理的新值。
			// 没把握时不要给这个字段，后端不会凭空生成修复提案。
			// field / oldValue / newValue 是旧字段，保留做兼容，
			// 新建议直接给 patches 即可，两个都给了时以 patches 为准。
			"suggestedRepair": map[string]any{"type": "object", "description": "可选的修复建议，用 patches 列表描述要改的字段，每个 Patch 是 op/path/value，op 只支持 replace，path 必须是白名单里的字段（镜像、内存、副本数），value 是合法的新值", "properties": map[string]any{
				"namespace":  map[string]any{"type": "string"},
				"deployment": map[string]any{"type": "string"},
				"container":  map[string]any{"type": "string"},
				"field":      map[string]any{"type": "string", "description": "旧字段，保留做兼容，新建议不用填"},
				"oldValue":   map[string]any{"type": "string", "description": "旧字段，保留做兼容，新建议不用填"},
				"newValue":   map[string]any{"type": "string", "description": "旧字段，保留做兼容，新建议不用填"},
				"reason":     map[string]any{"type": "string"},
				"patches": map[string]any{"type": "array", "description": "要改的字段列表，每个 Patch 是 op/path/value", "items": map[string]any{"type": "object", "properties": map[string]any{
					"op":    map[string]any{"type": "string", "description": "只支持 replace"},
					"path":  map[string]any{"type": "string", "description": "白名单里的字段路径，例如 /spec/template/spec/containers/0/image、/spec/template/spec/containers/0/resources/limits/memory、/spec/replicas"},
					"value": map[string]any{"description": "新值，例如 nginx:1.25、256Mi、3"},
				}}},
			}},
			// 创建建议是可选的（Slice 10）：只有用户明确要求创建
			// Deployment 时才给，用 namespace / name / image / replicas
			// 等字段描述要创建的 Deployment，不用先做故障诊断。
			// 用户说创建 Pod 时也按 Deployment 给。
			// 没问全信息（缺名字或镜像）时不要给这个字段，先问用户。
			"suggestedCreation": map[string]any{"type": "object", "description": "可选的创建建议：只有用户明确要求创建 Deployment 时才给，描述要创建的 Deployment，不用先做故障诊断；用户说创建 Pod 时也按 Deployment 给；信息没问全时不要给，先问用户", "properties": map[string]any{
				"namespace":     map[string]any{"type": "string", "description": "命名空间"},
				"name":          map[string]any{"type": "string", "description": "要创建的 Deployment 名"},
				"image":         map[string]any{"type": "string", "description": "镜像，例如 nginx:1.25"},
				"replicas":      map[string]any{"type": "number", "description": "副本数，1-10，默认 1"},
				"container":     map[string]any{"type": "string", "description": "容器名，默认和 Deployment 名相同"},
				"memoryLimit":   map[string]any{"type": "string", "description": "容器内存 limit，例如 128Mi，不填也可以"},
				"memoryRequest": map[string]any{"type": "string", "description": "容器内存 request，例如 64Mi，不填也可以"},
				"cpuLimit":      map[string]any{"type": "string", "description": "容器 CPU limit，例如 500m，不填也可以"},
				"cpuRequest":    map[string]any{"type": "string", "description": "容器 CPU request，例如 100m，不填也可以"},
				"reason":        map[string]any{"type": "string"},
			}},
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
2. 先查 Pod 状态、事件和日志，再查 Deployment 和手册，不要凭空猜测。不知道目标在哪时，先用发现工具（list_namespaces、list_problem_pods、get_cluster_overview）看全貌、找问题，再用定向工具深挖。
3. 证据不足时不要硬编结论，调用 submit_diagnosis 时把 confidence 写成 low，并写清还需要补查什么。
4. 提交诊断必须调用 submit_diagnosis，evidenceIds 只能引用工具结果里已经出现过的 ID。
5. 原因按证据自己判断。需要修复时，在 submit_diagnosis 的 suggestedRepair 里用 patches 列表给一个修复建议（op 只支持 replace，path 是白名单里的字段：镜像、内存、副本数，value 是合法的新值）；其他原因给不出具体变更时不要给修复建议。
6. 你只能诊断、不能执行任何修改：修复建议只是建议，是否执行由人工审批决定。
7. 用户如果明确说了要改成什么值（例如“换成 nginx:1.25”、“增加 128Mi”、“调到 256Mi”），按用户说的算新值：先查当前值，再按用户说的增量或目标值算，不要默认翻倍。用户上一轮已经明确说了新值时，直接用那个值生成结构化 Patch，不要再问一遍。
8. 用户明确说要创建 Deployment 或 Pod 时，先问缺的信息（命名空间、名字、镜像、副本数），问全了直接调用 submit_diagnosis，在 suggestedCreation 里给出创建建议，不用先做故障诊断。用户说创建 Pod 时也按 Deployment 给，并在报告里说明裸 Pod 不会自动重建、建议用 Deployment。`

// Next 问一次真模型，返回一步工具调用或最终报告。
func (r *Real) Next(ctx context.Context, in agent.ModelInput) (agent.ModelStep, error) {
	if strings.TrimSpace(r.getCfg().APIKey) == "" {
		return agent.ModelStep{}, fmt.Errorf("真模型缺少 API Key，请设置 MODEL_API_KEY")
	}
	msgs := buildRealMessages(in)
	// submit_diagnosis 的解析失败在这里先处理一遍：
	// 解析成功直接返回报告；解析失败纠错一次——把字段错误当 tool 结果
	// 回传（下面追加的一对 assistant+tool 消息），模型重交报告；
	// 纠错一次仍失败就降级为低置信度收敛报告，不让整轮作废。
	// 纠错是本轮内的一次额外模型调用（最多一次），不另开预算、
	// 不重新执行工具；追加的是 assistant（带失败的 tool_calls）+ tool
	// （字段错误）两条消息，是模型已主动交报告后的协议内错误反馈，
	// 不是 v0.13.0 删掉的追问 hack——绝不追加假 user 消息。
	// 循环最多跑两轮：第一轮解析失败会 continue，第二轮无论如何都返回，
	// 不会死循环。跳出循环时 msg 里已没有 submit_diagnosis。
	var msg chatMessage
	corrected := false
	// totalUsage 攒着本轮 Next 里所有模型调用的用量（token 用量统计）：
	// 纠错那次额外调用也算，解析失败那次也算——花了就是花了，
	// 不能因为最后只返回一份报告就把前面的用量丢了。
	// 统计口径：只要收到模型响应就记用量，包括纠错轮次与最终失败的那次；
	// 只有请求本身失败（没拿到响应）时无用量可记。所以下面所有出错返回
	// 都带上已攒的 totalUsage，调用方（Agent.Run）出错时也记账。
	var totalUsage store.Usage
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := r.callWithRetry(ctx, msgs, r.toolDefs())
		if err != nil {
			return agent.ModelStep{Usage: totalUsage}, err
		}
		totalUsage = totalUsage.Add(usageOf(resp))
		if len(resp.Choices) == 0 {
			return agent.ModelStep{Usage: totalUsage}, fmt.Errorf("模型返回为空，没有 choices")
		}
		msg = resp.Choices[0].Message
		var diag *chatToolCall
		for i := range msg.ToolCalls {
			if msg.ToolCalls[i].Function.Name == "submit_diagnosis" {
				diag = &msg.ToolCalls[i]
				break
			}
		}
		if diag == nil {
			break
		}
		rep, err := parseDiagnosisArgs(diag.Function.Arguments)
		if err == nil {
			return agent.ModelStep{IsFinal: true, Report: rep, Usage: totalUsage}, nil
		}
		r.logDiagnosisParseError(diag.Function.Arguments, err)
		if corrected {
			// 纠错一次仍失败：低置信度收敛报告，不让整轮作废。
			// 失败那轮附带的文字也留下来（已标注未经结构化验证）。
			return agent.ModelStep{IsFinal: true, Report: r.diagnosisFallbackReport(in, err, strings.TrimSpace(msg.Content)), Usage: totalUsage}, nil
		}
		corrected = true
		perr := &DiagnosisParseError{ToolCallID: diag.ID, ToolName: diag.Function.Name, Arguments: diag.Function.Arguments, Content: strings.TrimSpace(msg.Content), Err: err}
		msgs = append(msgs,
			chatMessage{Role: "assistant", Content: strings.TrimSpace(msg.Content), ToolCalls: []chatToolCall{newChatToolCall(diag.ID, diag.Function.Name, diag.Function.Arguments)}},
			chatMessage{Role: "tool", Content: perr.CorrectionFeedback(), ToolCallID: diag.ID},
		)
	}
	// 工具调用：一次返回里的多个调用按返回顺序全部执行，
	// 不能只取第一个、其余静默丢掉——模型以为它们都执行了。
	// submit_diagnosis 是终结信号：同一批里只要出现它，只处理它，
	// 跳过同批其他查询调用——交报告意味着本轮诊断结束，
	// 查询结果已无意义；继续执行查询只会浪费步数，
	// 还可能把无关证据带进报告。
	if len(msg.ToolCalls) > 0 {
		// 注意：submit_diagnosis 在上面已处理（解析成功直接返回报告，
		// 解析失败纠错一次或降级），到这里的调用里不会再有它。
		calls := make([]*agent.ToolCall, 0, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			name := tc.Function.Name
			args, err := parseToolArgs(tc.Function.Arguments)
			if err != nil {
				// 参数解析失败也带上已攒的用量：这轮的响应已经收到了，
				// 花掉的 token 不能因为参数解析失败就丢了。
				return agent.ModelStep{Usage: totalUsage}, fmt.Errorf("模型返回的工具参数解析失败: %w", err)
			}
			// 模型漏填 namespace 时补上任务的 namespace，缺了它 Agent 会拒绝执行。
			// 只给 schema 里声明了 namespace 的内置工具补（toolsWithNamespace）：
			// MCP 发现的外部工具只接受自己 InputSchema 声明的参数，
			// 多余的 namespace 会被 Server 拒绝（issue #11）。
			if _, ok := args["namespace"]; !ok || args["namespace"] == "" {
				if toolsWithNamespace[name] {
					args["namespace"] = in.Namespace
				}
			}
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call-%d-%d", in.StepIndex+1, i+1)
			}
			// 注意：这里不做最终校验，未知工具名会原样交给 Agent，
			// Agent 的 ValidateToolCall 会拒绝它并记一步错误，循环继续。
			calls = append(calls, &agent.ToolCall{ID: id, Name: name, Args: args})
		}
		return agent.ModelStep{ToolCalls: calls, Usage: totalUsage}, nil
	}
	// 没有工具调用时，把文本当作最终结论处理。
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		// 空回复也带上已攒的用量：响应已经收到了，用量是真实消耗。
		return agent.ModelStep{Usage: totalUsage}, fmt.Errorf("模型既没有调工具也没有返回文本")
	}
	if rep, err := tryParseReportJSON(content); err == nil {
		return agent.ModelStep{IsFinal: true, Report: rep, Usage: totalUsage}, nil
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
	}, Usage: totalUsage}, nil
}

// newChatToolCall 拼一次工具调用（纠错时把失败的 submit_diagnosis
// 原样放回 assistant 消息里，tool 结果才能对上号）。
func newChatToolCall(id, name, args string) chatToolCall {
	return chatToolCall{ID: id, Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: name, Arguments: args}}
}

// diagnosisFallbackReport 是 submit_diagnosis 纠错一次仍失败时的降级报告：
// 低置信度、用已有证据收敛，不让整轮作废。
// 写法和文本回退、Agent.finishWithPartial 同一路数。
// content 是失败那轮模型附带的文字（可能为空）：保留下来、
// 标明未经结构化验证，放在格式错误说明之后、证据摘要之前。
func (r *Real) diagnosisFallbackReport(in agent.ModelInput, parseErr error, content string) *agent.Report {
	ids := make([]string, 0, len(in.Evidence))
	for id := range in.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	errText := "未知解析错误"
	if parseErr != nil {
		errText = parseErr.Error()
	}
	facts := []string{"诊断报告格式有误：" + errText + "，已用已有证据收敛"}
	if strings.TrimSpace(content) != "" {
		facts = append(facts, UnverifiedAnalysisLabel+"：\n"+strings.TrimSpace(content))
	}
	for _, id := range ids {
		facts = append(facts, in.Evidence[id])
	}
	return &agent.Report{
		Facts:           facts,
		Cause:           "模型提交的诊断报告格式错误，无法确定根因",
		EvidenceIDs:     ids,
		Recommendations: []string{"修正报告格式后重新诊断，或补查 Pod 状态、事件和日志后再下结论"},
		OpenQuestions:   []string{"模型未用结构化方式提交诊断，结论需人工复核"},
		Confidence:      "low",
	}
}

// buildRealMessages 把任务和历史拼成发给模型的消息。
// 为什么要截断：日志全量已经很大，历史全塞进去会撑爆上下文，
// 还会烧很多 token。这里只传最近 6 步的摘要，每条最多 500 字，
// 全量证据仍在 SQLite 里，前端时间线能看到，模型不需要全量。
func buildRealMessages(in agent.ModelInput) []chatMessage {
	msgs := []chatMessage{
		{Role: "system", Content: realSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("任务：namespace=%s，类型=%s，名称=%s，问题：%s\n已走 %d 轮，请继续诊断。", in.Namespace, in.Kind, in.Name, in.Question, in.StepIndex)},
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
func (r *Real) callWithRetry(ctx context.Context, msgs []chatMessage, tools []chatToolDef) (*chatResponse, error) {
	cfg := r.getCfg()
	body, err := json.Marshal(chatRequest{
		Model: cfg.ModelName, Messages: msgs, Tools: tools,
		ToolChoice: "auto", Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
	})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(500*(1<<(attempt-1))) * time.Millisecond):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		resp, err := cfg.HTTPClient.Do(req)
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

// UnverifiedAnalysisLabel 是降级回复里附带模型原文时的标注：
// 这段文字没经过结构化验证，不能当已验证的诊断结论。
// 对话降级（runReal）和正式诊断降级（diagnosisFallbackReport）共用同一句。
const UnverifiedAnalysisLabel = "未经结构化验证的模型分析（仅供参考，不能作为已验证的诊断结论）"

// DiagnosisParseError 是 submit_diagnosis 参数解析失败的错误：
// 模型主动交了报告，但字段类型不符合 schema。
// 调用方可以拿它把字段错误当 tool 结果回传给模型纠错一次，
// 也可以据此降级为部分回复。它不是追问——
// 模型已经交过报告了，这里只是把解析错误按协议反馈回去。
type DiagnosisParseError struct {
	ToolCallID string // 失败那次 submit_diagnosis 调用的 ID
	ToolName   string // "submit_diagnosis"
	Arguments  string // 失败那次调用的原始参数
	Content    string // 模型同批返回的文字（可能为空）
	Err        error  // 字段级解析错误
}

func (e *DiagnosisParseError) Error() string {
	return fmt.Sprintf("模型提交的诊断报告解析失败: %v", e.Err)
}

func (e *DiagnosisParseError) Unwrap() error { return e.Err }

// CorrectionFeedback 是回传给模型的纠错内容：
// 字段错误 + 按正确格式重新提交的要求。
// 明确告诉模型不要重复调用已经执行过的工具——
// 纠错只补一对 assistant+tool 消息，不重新查任何东西。
func (e *DiagnosisParseError) CorrectionFeedback() string {
	return "submit_diagnosis 参数解析失败：" + e.Err.Error() + "。请按工具定义的格式重新提交诊断报告：facts、recommendations、openQuestions、evidenceIds 都是字符串数组，即使只有一条也要写成数组（例如 [\"……\"]）。不要重复调用已经执行过的工具，直接重新提交报告。"
}

// logDiagnosisParseError 在调试日志里记一条报告解析失败：
// 字段错误（含字段名和收到的类型）+ 截断后的原始参数片段。
// 只在 Debug 打开时调；参数里是诊断内容、不含密钥，
// 超长时截断到 500 字，不记完整响应。
func (r *Real) logDiagnosisParseError(raw string, err error) {
	if !r.getCfg().Debug {
		return
	}
	log.Printf("[debug model] submit_diagnosis 解析失败：%v；参数片段：%s", err, truncateRunes(raw, 500))
}

// jsonKindName 返回一段 JSON 值的类型名（string / number / boolean /
// object / array / null），给字段类型错误用。
func jsonKindName(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "无效 JSON"
	}
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return "未知类型"
	}
}

// parseStringList 解析报告里的字符串数组字段（facts、evidenceIds、
// recommendations、openQuestions）：
//   - 数组：原样返回；缺失或 null：当作没给；
//   - 单个字符串：包成单元素数组——模型偶尔会把只有一条的事实
//     写成裸字符串，这是最常见的格式走样，兼容它；
//   - 对象、数字、布尔等无法明确转换的类型：返回指明字段名的错误，
//     不要一律 fmt.Sprint 糊过去；数组里混了非字符串元素也一样报错。
func parseStringList(raw json.RawMessage, field string) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	if jsonKindName(raw) == "array" {
		return nil, fmt.Errorf("%s 数组元素应为字符串", field)
	}
	return nil, fmt.Errorf("%s 应为字符串数组，收到 %s", field, jsonKindName(raw))
}

// parseStringField 解析报告里的字符串字段（cause、confidence）：
// 类型不对时返回指明字段名的错误；缺失或 null 当作空字符串。
func parseStringField(raw json.RawMessage, field string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s 应为字符串，收到 %s", field, jsonKindName(raw))
	}
	return s, nil
}

// parseDiagnosisArgs 解析 submit_diagnosis 的参数。
// 先按 json.RawMessage 分层解码，再逐字段解析：
// 展示用数组字段收到单个字符串时兼容成单元素数组；
// 收到对象、数字等无法明确转换的类型时返回指明字段名的错误，
// 调用方据此纠错一次或降级。
// suggestedRepair / suggestedCreation 继续严格校验：
// 解析失败时丢弃该建议（报告其余部分照常返回），
// 绝不因此生成审批卡片、绝不自动生成或执行修复。
// validRepairSuggestion 校验修复建议的结构完整性：
// Normalize 之后 Patches 必须非空，每条 Patch 的 op 只能是 replace、
// path 非空、value 存在。目标字段（namespace / deployment / container）
// 不在这里要求：转换和提案生成时能从任务目标、会话历史或
// Deployment 的第一个容器兜底，缺了也不影响卡片转换。
// 只含旧字段但能转成合法 Patch 的，Normalize 之后同样合格。
// Path 白名单、目标合法性仍由 Worker 的 ValidatePatch 负责，不在这里重复。
func validRepairSuggestion(s *agent.SuggestedRepair) bool {
	if s == nil || len(s.Patches) == 0 {
		return false
	}
	for _, p := range s.Patches {
		if p.Op != "replace" || strings.TrimSpace(p.Path) == "" || p.Value == nil {
			return false
		}
	}
	return true
}

func parseDiagnosisArgs(raw string) (*agent.Report, error) {
	var dto struct {
		Facts           json.RawMessage `json:"facts"`
		Cause           json.RawMessage `json:"cause"`
		EvidenceIDs     json.RawMessage `json:"evidenceIds"`
		Recommendations json.RawMessage `json:"recommendations"`
		OpenQuestions   json.RawMessage `json:"openQuestions"`
		Confidence      json.RawMessage `json:"confidence"`
		SuggestedRepair json.RawMessage `json:"suggestedRepair"`
		SuggestedCreation json.RawMessage `json:"suggestedCreation"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &dto); err != nil {
		return nil, err
	}
	facts, err := parseStringList(dto.Facts, "facts")
	if err != nil {
		return nil, err
	}
	evidenceIDs, err := parseStringList(dto.EvidenceIDs, "evidenceIds")
	if err != nil {
		return nil, err
	}
	recommendations, err := parseStringList(dto.Recommendations, "recommendations")
	if err != nil {
		return nil, err
	}
	openQuestions, err := parseStringList(dto.OpenQuestions, "openQuestions")
	if err != nil {
		return nil, err
	}
	cause, err := parseStringField(dto.Cause, "cause")
	if err != nil {
		return nil, err
	}
	confidence, err := parseStringField(dto.Confidence, "confidence")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cause) == "" {
		return nil, fmt.Errorf("诊断报告缺少 cause")
	}
	if confidence == "" {
		confidence = "medium"
	}
	rep := &agent.Report{
		Facts: facts, Cause: cause, EvidenceIDs: evidenceIDs,
		Recommendations: recommendations, OpenQuestions: openQuestions,
		Confidence: confidence,
	}
	// 模型 JSON 缺字段时解出来是 nil，归一化成空数组再返回，
	// 否则序列化成 null，前端读 .length 会白屏（见 agent.Report.Normalize）。
	rep.Normalize()
	if len(dto.SuggestedRepair) > 0 && string(dto.SuggestedRepair) != "null" {
		var sr struct {
			Namespace  string `json:"namespace"`
			Deployment string `json:"deployment"`
			Container  string `json:"container"`
			Field      string `json:"field"`
			OldValue   string `json:"oldValue"`
			NewValue   string `json:"newValue"`
			Reason     string `json:"reason"`
			Patches    []struct {
				Op    string `json:"op"`
				Path  string `json:"path"`
				Value any    `json:"value"`
			} `json:"patches"`
		}
		if err := json.Unmarshal(dto.SuggestedRepair, &sr); err != nil {
			// 修复建议严格校验失败：丢弃该建议，不让整份报告作废，
			// 也不生成审批卡片；丢弃的原因写进 openQuestions，免得悄悄丢了。
			rep.OpenQuestions = append(rep.OpenQuestions, "模型给出的修复建议格式无效，已丢弃，未生成审批卡片")
		} else {
			s := &agent.SuggestedRepair{
				Namespace: sr.Namespace, Deployment: sr.Deployment,
				Container: sr.Container, Field: sr.Field,
				OldValue: sr.OldValue, NewValue: sr.NewValue,
				Reason: sr.Reason,
			}
			for _, p := range sr.Patches {
				s.Patches = append(s.Patches, agent.PatchOperation{Op: p.Op, Path: p.Path, Value: p.Value})
			}
			// 只有旧字段时转成通用 Patch，有 Patches 的直接保留。
			s.Normalize()
			if !validRepairSuggestion(s) {
				// 结构不完整（例如空对象 {}、Patches 为空、Patch 缺字段）：
				// 丢弃该建议，不生成审批卡片；原因写进 openQuestions。
				// 只验结构完整性，白名单、目标校验仍由 Worker 的
				// ValidatePatch / 提案生成负责。
				rep.OpenQuestions = append(rep.OpenQuestions, "模型给出的修复建议不完整，已丢弃，未生成审批卡片")
			} else {
				rep.SuggestedRepair = s
			}
		}
	}
	if len(dto.SuggestedCreation) > 0 && string(dto.SuggestedCreation) != "null" {
		// 创建建议（Slice 10）：只有用户明确要求创建 Deployment 时模型才给，
		// 字段缺一不可（namespace / name / image），缺了后面校验会清掉。
		var sc struct {
			Namespace     string  `json:"namespace"`
			Name          string  `json:"name"`
			Image         string  `json:"image"`
			Replicas      float64 `json:"replicas"`
			Container     string  `json:"container"`
			MemoryLimit   string  `json:"memoryLimit"`
			MemoryRequest string  `json:"memoryRequest"`
			CpuLimit      string  `json:"cpuLimit"`
			CpuRequest    string  `json:"cpuRequest"`
			Reason        string  `json:"reason"`
		}
		if err := json.Unmarshal(dto.SuggestedCreation, &sc); err != nil {
			// 创建建议严格校验失败：同样丢弃，不生成创建卡片。
			rep.OpenQuestions = append(rep.OpenQuestions, "模型给出的创建建议格式无效，已丢弃，未生成审批卡片")
		} else {
			sc := &agent.SuggestedCreation{
				Namespace: sc.Namespace, Name: sc.Name,
				Image: sc.Image, Replicas: int(sc.Replicas),
				Container:   sc.Container,
				MemoryLimit: sc.MemoryLimit, MemoryRequest: sc.MemoryRequest,
				CpuLimit: sc.CpuLimit, CpuRequest: sc.CpuRequest,
				Reason: sc.Reason,
			}
			// 创建建议的结构完整性：namespace、name、image 缺一不可
			//（与 repair.ValidateCreation 的要求一致）；replicas <= 0
			// 时下游默认按 1 处理，不算不合格。
			if sc.Namespace == "" || sc.Name == "" || sc.Image == "" {
				rep.OpenQuestions = append(rep.OpenQuestions, "模型给出的创建建议不完整，已丢弃，未生成审批卡片")
			} else {
				rep.SuggestedCreation = sc
			}
		}
	}
	return rep, nil
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

// ---------------------------------------------------------------------------
// 对话模式用的单步调用（Slice 4）
// ---------------------------------------------------------------------------

// ChatMessage 是对话模式发给模型的一条消息，
// 和 OpenAI 兼容协议的 chat message 一一对应，
// 只是字段导出，internal/chat 包才能组装历史。
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatToolCall 是模型在对话里返回的一次工具调用。
type ChatToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatStep 是 Real.Chat 一次调用的返回：
// 一次返回里文字和工具调用 / 诊断报告可以同时存在，
// Content 和 ToolCalls / Report 都带上，调用方都要保留——
// 模型的文字不能因为它同时调了工具、交了报告就被丢掉。
// ToolCalls 是一批：一次返回里的多个工具调用按返回顺序全部执行，
// 不能只取第一个、其余静默丢掉。
type ChatStep struct {
	Content   string
	ToolCalls []*agent.ToolCall
	Report    *agent.Report
	IsFinal   bool
	// Usage 是这一次模型调用的 token 用量（token 用量统计）：
	// 响应里没带 usage 时是零值，调用方只累加非零值。
	Usage store.Usage
}

// Chat 问一次真模型，专供对话循环用：
// 调用方把本会话的历史（最近 10 条）拼成 msgs 传进来，
// 模型返回一批工具调用或最终回答，调用方执行工具、
// 把结果以 tool 角色追加进历史，再调下一次。
// 工具表和单次诊断是同一套（含 4 个发现工具），
// 所以对话里模型能先看全貌再深挖；submit_diagnosis 在对话里
// 被当作一份结构化诊断结论返回，不会自动执行任何修改。
func (r *Real) Chat(ctx context.Context, msgs []ChatMessage) (ChatStep, error) {
	if strings.TrimSpace(r.getCfg().APIKey) == "" {
		return ChatStep{}, fmt.Errorf("真模型缺少 API Key，请设置 MODEL_API_KEY")
	}
	internal := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := chatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{ID: tc.ID, Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: tc.Name, Arguments: tc.Arguments}})
		}
		internal = append(internal, cm)
	}
	resp, err := r.callWithRetry(ctx, internal, r.toolDefs())
	if err != nil {
		return ChatStep{}, err
	}
	// 用量在拿到响应后立刻算出来：下面所有返回（成功、解析失败、
	// 参数解析失败、空回复）都带上它——只要收到响应就记用量，
	// 只有请求本身失败（没拿到响应）时无用量可记。
	// 解析失败时用量走返回的 ChatStep，不走 DiagnosisParseError，
	// 调用方（runReal）出错时也记账。
	u := usageOf(resp)
	if len(resp.Choices) == 0 {
		return ChatStep{Usage: u}, fmt.Errorf("模型返回为空，没有 choices")
	}
	msg := resp.Choices[0].Message
	// 文字和工具调用可以同时存在（模型一边说“我来查一下”一边调工具，
	// 或一边写结论一边交 submit_diagnosis）：先把文字取出来，
	// 下面无论返回工具调用还是诊断报告都带上，不丢。
	content := strings.TrimSpace(msg.Content)
	// 工具调用：一次返回里的多个调用按返回顺序全部执行，
	// 不能只取第一个、其余静默丢掉——模型以为它们都执行了。
	// submit_diagnosis 是终结信号：同一批里只要出现它，只处理它，
	// 跳过同批其他查询调用（和 Real.Next 同样处理，见上）。
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name == "submit_diagnosis" {
				rep, err := parseDiagnosisArgs(tc.Function.Arguments)
				if err != nil {
					// 解析失败不直接吞成普通错误：调用方（runReal）要拿
					// 这个错误纠错一次（把字段错误当 tool 结果回传），
					// 纠错仍失败再降级为部分回复。纠错不在这里做——
					// 发给模型的消息数组在调用方手里，那里追加才对。
					r.logDiagnosisParseError(tc.Function.Arguments, err)
					return ChatStep{Usage: u}, &DiagnosisParseError{ToolCallID: tc.ID, ToolName: tc.Function.Name, Arguments: tc.Function.Arguments, Content: content, Err: err}
				}
				return ChatStep{IsFinal: true, Content: content, Report: rep, Usage: u}, nil
			}
		}
		calls := make([]*agent.ToolCall, 0, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			args, err := parseToolArgs(tc.Function.Arguments)
			if err != nil {
				// 参数解析失败也带上用量：这轮的响应已经收到了，
				// 花掉的 token 不能因为参数解析失败就丢了。
				return ChatStep{Usage: u}, fmt.Errorf("模型返回的工具参数解析失败: %w", err)
			}
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("call-chat-%d", i+1)
			}
			calls = append(calls, &agent.ToolCall{ID: id, Name: tc.Function.Name, Args: args})
		}
		return ChatStep{Content: content, ToolCalls: calls, Usage: u}, nil
	}
	if content == "" {
		return ChatStep{Usage: u}, fmt.Errorf("模型既没有调工具也没有返回文本")
	}
	return ChatStep{IsFinal: true, Content: content, Usage: u}, nil
}
