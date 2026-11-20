// Package api 是 HTTP API 层：校验请求、读写任务、接收审批决定。
//
// 契约见架构文档。注意：API 层不做诊断、不调模型、不碰 K8s，
// 只负责把请求翻译成 Store 操作，这样单测不需要起集群。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"kubecause/internal/agent"
	"kubecause/internal/chat"
	"kubecause/internal/config"
	"kubecause/internal/store"
)

// Handler 持有 Store。
// Chat 是对话服务（Slice 4），为 nil 时会话接口返回 503，
// 单次诊断接口不受影响。
// Config 是配置中心（Slice 8），为 nil 时配置接口返回 503。
// OnConfigChange 在 POST /api/config 成功后调用，
// 由 main.go 把新配置应用到对话服务、任务默认值、修复上限和真模型，
// 热加载不用重启。
type Handler struct {
	Store          *store.Store
	Chat           *chat.Service
	Config         *config.Manager
	OnConfigChange func(config.Config)
}

// CreateRunRequest 是 POST /api/runs 的请求体。
type CreateRunRequest struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // Pod 或 Deployment
	Name      string `json:"name"`
	Question  string `json:"question"`
	// MaxSteps 和 TimeoutSeconds 可选，不填用默认值 8 步 / 60 秒。
	MaxSteps       int `json:"maxSteps,omitempty"`
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

var (
	nsRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// validateCreate 校验创建任务的请求。
// namespace 必须和 K8s 命名规则一致，kind 只能是 Pod 或 Deployment，
// 名字不能为空。校验放在 API 层，Worker 就不用再防一遍非法输入。
// 系统命名空间不再拒绝：单次诊断是只读诊断，查 kube-system 合法；
// 修操作仍然被挡住——提案生成时 repair.ValidatePatchSpec 会拒绝
// 系统命名空间，所以系统目标的任务只会出诊断报告，不会生成可执行提案。
func validateCreate(req *CreateRunRequest) error {
	if !nsRe.MatchString(req.Namespace) {
		return fmt.Errorf("namespace 格式非法: %q", req.Namespace)
	}
	if req.Kind != "Pod" && req.Kind != "Deployment" {
		return fmt.Errorf("kind 只能是 Pod 或 Deployment")
	}
	if req.Name == "" || !nameRe.MatchString(req.Name) {
		return fmt.Errorf("name 格式非法: %q", req.Name)
	}
	if strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("question 不能为空")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// CreateRun 处理 POST /api/runs，创建任务后返回 202 和 runId。
// 任务是异步执行的：API 只写库、不跑诊断，Worker 随后领取，
// 所以浏览器关闭页面后任务还会继续。
func (h *Handler) CreateRun(w http.ResponseWriter, r *http.Request) {
	var req CreateRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := validateCreate(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run := &store.Run{
		Namespace: req.Namespace, Kind: req.Kind, Name: req.Name,
		Question: req.Question, MaxSteps: req.MaxSteps, TimeoutSeconds: req.TimeoutSeconds,
	}
	if err := h.Store.CreateRun(r.Context(), run); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": run.ID, "status": run.Status})
}

// RunSnapshot 是 GET /api/runs/{id} 的返回：任务状态、诊断报告、
// 待审批提案和修复状态。前端刷新后用 runId 重新读这个接口就能恢复页面。
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := h.Store.GetRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	var report *agent.Report
	if run.ReportJSON != "" {
		var rep agent.Report
		if err := json.Unmarshal([]byte(run.ReportJSON), &rep); err == nil {
			report = &rep
		}
	}
	steps, _ := h.Store.ListSteps(r.Context(), id)
	proposal, _ := h.Store.GetChangeRequestByRun(r.Context(), id)
	resp := map[string]any{
		"id": run.ID, "namespace": run.Namespace, "kind": run.Kind, "name": run.Name,
		"question": run.Question, "status": run.Status,
		"maxSteps": run.MaxSteps, "timeoutSeconds": run.TimeoutSeconds,
		"report": report, "error": run.Error,
		"steps": steps, "stepCount": len(steps),
		"proposal": proposal,
		// 执行和验证分开返回：repairStatus 是写入结果，
		// verificationStatus 是 rollout 验证结果，两者不要混成一个。
		"repairStatus":       repairStatusOf(proposal),
		"verificationStatus": verificationStatusOf(proposal),
		"executionDetail":    parseDetail(proposal, true),
		"verificationDetail": parseDetail(proposal, false),
		"createdAt":          run.CreatedAt, "updatedAt": run.UpdatedAt,
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseDetail 把提案里存的执行/验证结果 JSON 字符串解析成对象，
// 前端直接当对象用，不用自己再 JSON.parse 一次。没有结果时返回 nil。
func parseDetail(p *store.ChangeRequest, execution bool) any {
	if p == nil {
		return nil
	}
	raw := p.VerificationDetail
	if execution {
		raw = p.ExecutionDetail
	}
	if raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return v
}

func verificationStatusOf(p *store.ChangeRequest) string {
	if p == nil {
		return "none"
	}
	return p.VerificationStatus
}

func repairStatusOf(p *store.ChangeRequest) string {
	if p == nil {
		return "none"
	}
	return p.ExecutionStatus
}

// ListEvents 处理 GET /api/runs/{id}/events?after=42，
// 返回序号大于 after 的时间线事件，供前端增量轮询，不用每次拉全量。
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.Store.GetRun(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "任务不存在")
		return
	}
	after := 0
	if s := r.URL.Query().Get("after"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after 必须是非负整数")
			return
		}
		after = n
	}
	steps, err := h.Store.ListStepsAfter(r.Context(), id, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if steps == nil {
		steps = []store.RunStep{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runId": id, "after": after, "events": steps})
}

// DecisionRequest 是审批决定的请求体。
type DecisionRequest struct {
	Decision string `json:"decision"` // approved 或 rejected
	Approver string `json:"approver"`
}

// DecideApproval 处理 POST /api/runs/{id}/approvals/{approvalId}/decision。
// 只记录审批决定，不在这里执行修复：批准后 Worker 循环会领取提案、
// 执行 Patch 并验证 rollout；拒绝后 Worker 永远不会领取它，
// 所以拒绝后无写入。重复提交返回当前结果，不重复改状态。
func (h *Handler) DecideApproval(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	approvalID := r.PathValue("approvalId")
	var req DecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Decision != store.ChangeApproved && req.Decision != store.ChangeRejected {
		writeError(w, http.StatusBadRequest, "decision 只能是 approved 或 rejected")
		return
	}
	// 先核对提案和任务的绑定关系，防止拿 A 任务的提案去批 B 任务。
	proposal, err := h.Store.GetChangeRequest(r.Context(), approvalID)
	if err != nil {
		writeError(w, http.StatusNotFound, "提案不存在")
		return
	}
	if proposal.RunID != runID {
		writeError(w, http.StatusBadRequest, "提案与任务不匹配")
		return
	}
	decided, already, err := h.Store.DecideApproval(r.Context(), approvalID, req.Decision, req.Approver)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approvalId": decided.ID, "runId": decided.RunID, "status": decided.Status,
		"approver": decided.Approver, "alreadyDecided": already,
		"note": "审批决定已记录。批准后 Worker 将执行 Patch 并验证 rollout；拒绝后不会执行，不会对集群做任何写入",
	})
}

// Register 把路由挂到 mux 上。Go 1.22+ 的 ServeMux 支持方法 + 路径模式。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs", h.CreateRun)
	mux.HandleFunc("GET /api/runs/{id}", h.GetRun)
	mux.HandleFunc("GET /api/runs/{id}/events", h.ListEvents)
	mux.HandleFunc("POST /api/runs/{id}/approvals/{approvalId}/decision", h.DecideApproval)
	// 对话模式（Slice 4）：只读多轮对话，转正式诊断才走 /api/runs。
	mux.HandleFunc("POST /api/conversations", h.CreateConversation)
	mux.HandleFunc("GET /api/conversations", h.ListConversations)
	mux.HandleFunc("GET /api/conversations/{id}", h.GetConversation)
	mux.HandleFunc("POST /api/conversations/{id}/messages", h.PostConversationMessage)
	mux.HandleFunc("POST /api/conversations/{id}/convert", h.ConvertConversation)
	// 配置中心（Slice 8）：读当前配置、改配置并热加载。
	// 配置里没有 MODEL_API_KEY 这个字段——Key 只从环境变量读，
	// 绝不进配置文件、绝不在这里返回、绝不在前端显示。
	mux.HandleFunc("GET /api/config", h.GetConfig)
	mux.HandleFunc("POST /api/config", h.UpdateConfig)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// ---------------------------------------------------------------------------
// 对话模式（Slice 4）
// ---------------------------------------------------------------------------

func (h *Handler) requireChat(w http.ResponseWriter) bool {
	if h.Chat == nil {
		writeError(w, http.StatusServiceUnavailable, "对话服务未配置")
		return false
	}
	return true
}

// CreateConversation 处理 POST /api/conversations，创建一个空会话。
func (h *Handler) CreateConversation(w http.ResponseWriter, r *http.Request) {
	if !h.requireChat(w) {
		return
	}
	c, err := h.Store.CreateConversation(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "title": c.Title, "createdAt": c.CreatedAt})
}

// ListConversations 处理 GET /api/conversations，列出会话。
func (h *Handler) ListConversations(w http.ResponseWriter, r *http.Request) {
	list, err := h.Store.ListConversations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

// GetConversation 处理 GET /api/conversations/{id}，读会话历史。
func (h *Handler) GetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.Store.GetConversation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	msgs, _ := h.Store.ListMessages(r.Context(), id)
	if msgs == nil {
		msgs = []store.ConversationMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "title": c.Title, "messages": msgs})
}

// PostMessageRequest 是 POST /api/conversations/{id}/messages 的请求体。
type PostMessageRequest struct {
	Content string `json:"content"`
}

// PostConversationMessage 处理 POST /api/conversations/{id}/messages，
// 发一条用户消息，跑一轮 Agent，返回助手回复、本轮工具和结构化结果。
// 对话只读：本接口不生成 change_requests、不执行任何修改。
func (h *Handler) PostConversationMessage(w http.ResponseWriter, r *http.Request) {
	if !h.requireChat(w) {
		return
	}
	id := r.PathValue("id")
	var req PostMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content 不能为空")
		return
	}
	reply, err := h.Chat.RunMessage(r.Context(), id, req.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

// ConvertRequest 是 POST /api/conversations/{id}/convert 的请求体，
// 全部字段可选：不填时按会话历史解析目标，解析不到就报错请明确指定。
type ConvertRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Question  string `json:"question,omitempty"`
}

// ConvertConversation 处理 POST /api/conversations/{id}/convert，
// 把对话创建可审批的修复任务任务，返回 runId。转换本身不执行修复，
// 修复还是要走提案 + 人工审批 + 执行验证。
func (h *Handler) ConvertConversation(w http.ResponseWriter, r *http.Request) {
	if !h.requireChat(w) {
		return
	}
	id := r.PathValue("id")
	var req ConvertRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	runID, err := h.Chat.ConvertToRun(r.Context(), id, req.Namespace, req.Kind, req.Name, req.Question)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": runID, "conversationId": id})
}

// ---------------------------------------------------------------------------
// 配置中心（Slice 8）
// ---------------------------------------------------------------------------

// GetConfig 处理 GET /api/config，返回当前生效的配置。
// 返回里没有 MODEL_API_KEY：配置结构本身就没有 Key 这个字段，
// Key 只从环境变量读，前端也拿不到它。
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	if h.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "配置中心未配置")
		return
	}
	writeJSON(w, http.StatusOK, h.Config.Current())
}

// UpdateConfig 处理 POST /api/config：
// 校验新配置 → 写回 config.yaml → 换掉内存里的当前值 → 通知各组件热加载。
// 校验失败返回 400，旧配置继续生效，文件不被改写。
func (h *Handler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	if h.Config == nil {
		writeError(w, http.StatusServiceUnavailable, "配置中心未配置")
		return
	}
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if err := h.Config.Update(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.OnConfigChange != nil {
		h.OnConfigChange(cfg)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config": cfg,
		"note":   "已保存，立即生效，不用重启",
	})
}

// 确保 context 被用到（以后接权限校验时用）。
var _ = context.Background
