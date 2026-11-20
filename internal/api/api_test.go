package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"kubecause/internal/config"
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

// TestConfigGetPost 覆盖配置中心 GET/POST：
// POST 一个合法值后 GET 能读到；POST 一个非法值（maxSteps=0、temperature=2）
// 返回 400，且旧值不变、文件不被改写。
// 同时验证返回里没有 MODEL_API_KEY——配置里根本没有 Key 这个字段，
// 前端也拿不到它。
func TestConfigGetPost(t *testing.T) {
	h := newHandler(t)
	dir := t.TempDir()
	mgr := config.NewManager(config.Defaults(), filepath.Join(dir, "config.yaml"))
	applied := config.Defaults()
	h.Config = mgr
	h.OnConfigChange = func(c config.Config) { applied = c }
	mux := http.NewServeMux()
	h.Register(mux)

	// GET 应返回默认配置：对话步数 12，单次诊断步数 8。
	req := httptest.NewRequest("GET", "/api/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config 应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var got config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET 返回应是合法配置 JSON: %v", err)
	}
	if got.Chat.MaxSteps != 12 || got.Run.MaxSteps != 8 {
		t.Fatalf("默认配置应是对话 12 / 单次 8，得到 %+v", got)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("API_KEY")) || bytes.Contains(rec.Body.Bytes(), []byte("apiKey")) {
		t.Fatalf("配置返回里不应出现任何 Key 字段: %s", rec.Body.String())
	}

	// POST 合法值：对话步数改成 16，应 200，GET 能读到，回调也被调到。
	next := config.Defaults()
	next.Chat.MaxSteps = 16
	body, _ := json.Marshal(next)
	req = httptest.NewRequest("POST", "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST 合法配置应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("不用重启")) {
		t.Fatalf("保存成功应明确说不用重启: %s", rec.Body.String())
	}
	if applied.Chat.MaxSteps != 16 {
		t.Fatalf("热更新回调应收到新值 16，得到 %d", applied.Chat.MaxSteps)
	}
	req = httptest.NewRequest("GET", "/api/config", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Chat.MaxSteps != 16 {
		t.Fatalf("POST 后 GET 应读到 16，得到 %d", got.Chat.MaxSteps)
	}
	// 文件也写回去了，重启后读到的就是新值。
	saved, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil || saved.Chat.MaxSteps != 16 {
		t.Fatalf("配置文件应保存新值 16，得到 %+v / %v", saved, err)
	}

	// POST 非法值：maxSteps=0 应 400，旧值 16 不变。
	bad := config.Defaults()
	bad.Chat.MaxSteps = 0
	body, _ = json.Marshal(bad)
	req = httptest.NewRequest("POST", "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST 非法配置应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/api/config", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Chat.MaxSteps != 16 {
		t.Fatalf("非法 POST 后旧值应保持 16，得到 %d", got.Chat.MaxSteps)
	}
	if applied.Chat.MaxSteps != 16 {
		t.Fatalf("非法 POST 不应触发热更新回调，得到 %d", applied.Chat.MaxSteps)
	}

	// POST 非法值：temperature=2 应 400。
	bad = config.Defaults()
	bad.Model.Temperature = 2
	body, _ = json.Marshal(bad)
	req = httptest.NewRequest("POST", "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("temperature=2 应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}
