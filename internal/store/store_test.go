package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGetRun(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if r.ID == "" || r.Status != StatusQueued {
		t.Fatalf("任务初始状态不对: %+v", r)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.Name != "demo-api" {
		t.Fatalf("读取任务失败: %v %+v", err, got)
	}
}

func TestClaimAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	claimed, err := s.ClaimNext(ctx)
	if err != nil || claimed == nil || claimed.Status != StatusRunning {
		t.Fatalf("领取任务失败: %v %+v", err, claimed)
	}
	// 第二次领取应为空，同一任务不能被领两次。
	if again, _ := s.ClaimNext(ctx); again != nil {
		t.Fatalf("同一任务被重复领取")
	}
	// 模拟进程崩溃：running 的任务在重启后应改回 queued，步骤保留。
	n, err := s.ResetRunningToQueued(ctx)
	if err != nil || n != 1 {
		t.Fatalf("重启恢复失败: n=%d err=%v", n, err)
	}
	claimed2, err := s.ClaimNext(ctx)
	if err != nil || claimed2 == nil || claimed2.ID != r.ID {
		t.Fatalf("恢复后无法重新领取: %v %+v", err, claimed2)
	}
}

func TestAppendStepAndListAfter(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	for i := 1; i <= 3; i++ {
		seq, _ := s.NextSeq(ctx, r.ID)
		if seq != i {
			t.Fatalf("序号不对: 想要 %d 得到 %d", i, seq)
		}
		if err := s.AppendStep(ctx, &RunStep{RunID: r.ID, Seq: seq, Kind: "tool_result", ToolName: "get_pod", Summary: "ok"}); err != nil {
			t.Fatalf("写步骤失败: %v", err)
		}
	}
	after, err := s.ListStepsAfter(ctx, r.ID, 1)
	if err != nil || len(after) != 2 || after[0].Seq != 2 {
		t.Fatalf("增量拉取不对: %v %+v", err, after)
	}
}

func TestDecideApprovalIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	c, err := s.CreateChangeRequest(ctx, &ChangeRequest{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi"})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	decided, already, err := s.DecideApproval(ctx, c.ID, ChangeApproved, "varo")
	if err != nil || already || decided.Status != ChangeApproved {
		t.Fatalf("第一次审批不对: %v already=%v %+v", err, already, decided)
	}
	// 重复提交（甚至换一个决定）必须返回当前结果，不改状态。
	decided2, already2, err := s.DecideApproval(ctx, c.ID, ChangeRejected, "varo")
	if err != nil || !already2 || decided2.Status != ChangeApproved {
		t.Fatalf("重复审批应幂等返回已批准: %v already=%v %+v", err, already2, decided2)
	}
}

func TestClaimApprovedChangeAndResetExecuting(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	_ = s.CreateRun(ctx, r)
	c, err := s.CreateChangeRequest(ctx, &ChangeRequest{RunID: r.ID, Namespace: "demo", Deployment: "demo-api", Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi"})
	if err != nil {
		t.Fatalf("创建提案失败: %v", err)
	}
	// 未批准时领不到。
	if claimed, _ := s.ClaimApprovedChange(ctx); claimed != nil {
		t.Fatalf("未批准的提案不应被领到")
	}
	if _, _, err := s.DecideApproval(ctx, c.ID, ChangeApproved, "varo"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	claimed, err := s.ClaimApprovedChange(ctx)
	if err != nil || claimed == nil || claimed.ExecutionStatus != ExecExecuting {
		t.Fatalf("批准后应能领到并标为 executing: %v %+v", err, claimed)
	}
	// 第二次领不到，同一提案不能被领两次。
	if again, _ := s.ClaimApprovedChange(ctx); again != nil {
		t.Fatalf("执行中的提案不应被重复领取")
	}
	// 模拟进程在执行中途崩掉：重启后应改回待执行。
	n, err := s.ResetExecutingChanges(ctx)
	if err != nil || n != 1 {
		t.Fatalf("重启恢复失败: n=%d err=%v", n, err)
	}
	claimed2, err := s.ClaimApprovedChange(ctx)
	if err != nil || claimed2 == nil || claimed2.ID != c.ID {
		t.Fatalf("恢复后应能重新领取: %v %+v", err, claimed2)
	}
}

func TestMigrateAddsRoundAndEvidenceColumns(t *testing.T) {
	// 模拟修复前的老库：先用裸 SQL 建不带 round / evidence_id /
	// max_tool_calls_per_round 的表并写一行老数据，再 Open——
	// migrate 应自动补列，老数据不丢，Round 读出来是 -1（未知/老数据哨兵）。
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建老库失败: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY, namespace TEXT NOT NULL, kind TEXT NOT NULL,
			name TEXT NOT NULL, question TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			max_steps INTEGER NOT NULL DEFAULT 8,
			timeout_seconds INTEGER NOT NULL DEFAULT 60,
			report_json TEXT NOT NULL DEFAULT '',
			suggested_json TEXT NOT NULL DEFAULT '',
			creation_json TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
		);
		CREATE TABLE run_steps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES runs(id),
			seq INTEGER NOT NULL, kind TEXT NOT NULL,
			tool_name TEXT NOT NULL DEFAULT '',
			call_id TEXT NOT NULL DEFAULT '',
			args_json TEXT NOT NULL DEFAULT '',
			result_json TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			retry_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			UNIQUE(run_id, seq)
		);
		INSERT INTO runs(id, namespace, kind, name, question, status, created_at, updated_at)
		VALUES('run_old', 'demo', 'Deployment', 'demo-api', '为什么重启', 'queued', '2026-11-22 00:00:00', '2026-11-22 00:00:00');
		INSERT INTO run_steps(run_id, seq, kind, tool_name, call_id, summary, created_at)
		VALUES('run_old', 1, 'tool_result', 'get_pod', 'call-1', 'Pod ok', '2026-11-22 00:00:00');
	`)
	if err != nil {
		t.Fatalf("写老数据失败: %v", err)
	}
	_ = raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	defer s.Close()
	// 老数据读出来：Round 是 -1（未知/老数据哨兵），EvidenceID 为空。
	steps, err := s.ListSteps(ctx, "run_old")
	if err != nil || len(steps) != 1 {
		t.Fatalf("老步骤应读出 1 条: %v %+v", err, steps)
	}
	if steps[0].Round != -1 || steps[0].EvidenceID != "" {
		t.Fatalf("老步骤的 Round 应是 -1、EvidenceID 为空，得到 %+v", steps[0])
	}
	// 新写入带 Round / EvidenceID 的步骤能 round-trip。
	if err := s.AppendStep(ctx, &RunStep{RunID: "run_old", Seq: 2, Kind: "tool_result", ToolName: "get_events", CallID: "call-2", Summary: "events", Round: 1, EvidenceID: "ev-1"}); err != nil {
		t.Fatalf("写新步骤失败: %v", err)
	}
	steps, _ = s.ListSteps(ctx, "run_old")
	if len(steps) != 2 || steps[1].Round != 1 || steps[1].EvidenceID != "ev-1" {
		t.Fatalf("新步骤的 Round / EvidenceID 应 round-trip，得到 %+v", steps)
	}
	// 新任务的 MaxToolCallsPerRound 默认 5。
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if r.MaxToolCallsPerRound != 5 {
		t.Fatalf("新任务的每轮调用上限默认应是 5，得到 %d", r.MaxToolCallsPerRound)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.MaxToolCallsPerRound != 5 {
		t.Fatalf("读回的任务每轮调用上限应是 5: %v %+v", err, got)
	}
}
