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
	"kubecause/internal/store"
)

// Handler 持有 Store。
type Handler struct {
	Store *store.Store
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
func validateCreate(req *CreateRunRequest) error {
	if !nsRe.MatchString(req.Namespace) {
		return fmt.Errorf("namespace 格式非法: %q", req.Namespace)
	}
	if req.Namespace == "kube-system" || req.Namespace == "kube-public" {
		return fmt.Errorf("禁止在系统命名空间 %q 创建任务", req.Namespace)
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
		// repairStatus 在 Slice 1 只有 not_started，执行在 Slice 3。
		"repairStatus": repairStatusOf(proposal),
		"createdAt":    run.CreatedAt, "updatedAt": run.UpdatedAt,
	}
	writeJSON(w, http.StatusOK, resp)
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
// Slice 1 只做持久化记录并明确返回“Slice 3 才执行修复”，
// 不假装已经执行。重复提交返回当前结果，不重复改状态。
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
		"note": "Slice 1 只记录审批决定，修复执行在 Slice 3，当前不会对集群做任何写入",
	})
}

// Register 把路由挂到 mux 上。Go 1.22+ 的 ServeMux 支持方法 + 路径模式。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs", h.CreateRun)
	mux.HandleFunc("GET /api/runs/{id}", h.GetRun)
	mux.HandleFunc("GET /api/runs/{id}/events", h.ListEvents)
	mux.HandleFunc("POST /api/runs/{id}/approvals/{approvalId}/decision", h.DecideApproval)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

// 确保 context 被用到（以后接权限校验时用）。
var _ = context.Background
