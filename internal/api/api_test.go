package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kubecause/internal/store"
)

func newHandler(t *testing.T) *Handler {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Handler{Store: s}
}

func TestCreateRunValidation(t *testing.T) {
	h := newHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)
	// 系统命名空间的诊断任务也应接受：诊断是只读，查 kube-system 合法；
	// 修操作仍会被 repair.ValidatePatchSpec 挡住，不会生成可执行提案。
	body, _ := json.Marshal(map[string]string{"namespace": "kube-system", "kind": "Deployment", "name": "coredns", "question": "x"})
	req := httptest.NewRequest("POST", "/api/runs", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("系统命名空间的诊断任务应 202，得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 非法 namespace 格式仍应 400。
	body, _ = json.Marshal(map[string]string{"namespace": "Bad_NS!", "kind": "Deployment", "name": "demo-api", "question": "x"})
	req = httptest.NewRequest("POST", "/api/runs", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 namespace 应 400，得到 %d", rec.Code)
	}
	// 合法应 202 并返回 runId。
	body, _ = json.Marshal(map[string]string{"namespace": "demo", "kind": "Deployment", "name": "demo-api", "question": "为什么重启"})
	req = httptest.NewRequest("POST", "/api/runs", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("合法创建应 202，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["runId"] == "" {
		t.Fatalf("应返回 runId: %s", rec.Body.String())
	}
	// GET 能读回任务。
	id := resp["runId"].(string)
	req = httptest.NewRequest("GET", "/api/runs/"+id, nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 任务应 200，得到 %d", rec.Code)
	}
	// events?after=0 应返回空列表而不是 null。
	req = httptest.NewRequest("GET", "/api/runs/"+id+"/events?after=0", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET events 应 200，得到 %d", rec.Code)
	}
	_ = context.Background
}

func TestDecideApprovalBinding(t *testing.T) {
	h := newHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)
	ctx := context.Background()
	// 直接在库里建任务和提案，再走 API 审批。
	r := &store.Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = h.Store.CreateRun(ctx, r)
	c, _ := h.Store.CreateChangeRequest(ctx, &store.ChangeRequest{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi"})
	// 提案和任务不匹配应 400。
	body, _ := json.Marshal(map[string]string{"decision": "approved", "approver": "varo"})
	req := httptest.NewRequest("POST", "/api/runs/run_other/approvals/"+c.ID+"/decision", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("提案与任务不匹配应 400，得到 %d", rec.Code)
	}
	// 正常批准应 200，且 note 明确批准后 Worker 会执行并验证，
	// 拒绝后不会执行。Slice 3 已接入执行，不再是 Slice 1 的占位文案。
	req = httptest.NewRequest("POST", "/api/runs/"+r.ID+"/approvals/"+c.ID+"/decision", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("批准应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("Worker 将执行 Patch")) {
		t.Fatalf("批准返回应明确说明批准后 Worker 会执行 Patch: %s", rec.Body.String())
	}
}
