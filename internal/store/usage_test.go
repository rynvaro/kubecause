package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestUsageColumnsMigratedForOldDB(t *testing.T) {
	// 模拟用量统计之前的老库：runs 和 conversations 表都没有
	// prompt_tokens / completion_tokens / total_tokens 三列，
	// 写一行老数据再 Open——migrate 应自动补列，老数据不丢，
	// 用量读出来是 0，之后累加能正常写。
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
		CREATE TABLE conversations (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
		);
		INSERT INTO runs(id, namespace, kind, name, question, status, created_at, updated_at)
		VALUES('run_old', 'demo', 'Deployment', 'demo-api', '为什么重启', 'queued', '2026-11-22 00:00:00', '2026-11-22 00:00:00');
		INSERT INTO conversations(id, title, created_at, updated_at)
		VALUES('conv_old', '', '2026-11-22 00:00:00', '2026-11-22 00:00:00');
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
	// 老数据读出来：用量是零值。
	r, err := s.GetRun(ctx, "run_old")
	if err != nil {
		t.Fatalf("读老任务失败: %v", err)
	}
	if !r.Usage.IsZero() {
		t.Fatalf("老任务的用量应是零值，得到 %+v", r.Usage)
	}
	c, err := s.GetConversation(ctx, "conv_old")
	if err != nil {
		t.Fatalf("读老会话失败: %v", err)
	}
	if !c.Usage.IsZero() {
		t.Fatalf("老会话的用量应是零值，得到 %+v", c.Usage)
	}
	// 补列后累加能正常写。
	if err := s.AddRunUsage(ctx, "run_old", Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("老库累加任务用量失败: %v", err)
	}
	r, _ = s.GetRun(ctx, "run_old")
	if want := (Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); r.Usage != want {
		t.Fatalf("老库累加后任务用量应为 %+v，得到 %+v", want, r.Usage)
	}
	if err := s.AddConversationUsage(ctx, "conv_old", Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}); err != nil {
		t.Fatalf("老库累加会话用量失败: %v", err)
	}
	c, _ = s.GetConversation(ctx, "conv_old")
	if want := (Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}); c.Usage != want {
		t.Fatalf("老库累加后会话用量应为 %+v，得到 %+v", want, c.Usage)
	}
}

func TestAddRunUsageAccumulates(t *testing.T) {
	// 两次累加要加起来；用量为零时不写库（也不报错），
	// total 缺失时按输入+输出补齐。
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	r := &Run{Namespace: "demo", Kind: "Deployment", Name: "demo-api", Question: "为什么重启"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := s.AddRunUsage(ctx, r.ID, Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}); err != nil {
		t.Fatalf("累加用量失败: %v", err)
	}
	if err := s.AddRunUsage(ctx, r.ID, Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("第二次累加用量失败: %v", err)
	}
	// 零值不写库：调用后总量不变，也不报错。
	if err := s.AddRunUsage(ctx, r.ID, Usage{}); err != nil {
		t.Fatalf("零用量不应报错: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if want := (Usage{PromptTokens: 110, CompletionTokens: 55, TotalTokens: 165}); got.Usage != want {
		t.Fatalf("两次用量应累加为 %+v，得到 %+v", want, got.Usage)
	}
	// total 缺失时按输入+输出补齐。
	if err := s.AddRunUsage(ctx, r.ID, Usage{PromptTokens: 80, CompletionTokens: 20}); err != nil {
		t.Fatalf("累加用量失败: %v", err)
	}
	got, _ = s.GetRun(ctx, r.ID)
	if want := (Usage{PromptTokens: 190, CompletionTokens: 75, TotalTokens: 265}); got.Usage != want {
		t.Fatalf("total 缺失的一次应按输入+输出补齐，累计应为 %+v，得到 %+v", want, got.Usage)
	}
}

func TestAddConversationUsageAccumulates(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	c, err := s.CreateConversation(ctx, "")
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := s.AddConversationUsage(ctx, c.ID, Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}); err != nil {
		t.Fatalf("累加用量失败: %v", err)
	}
	if err := s.AddConversationUsage(ctx, c.ID, Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("第二次累加用量失败: %v", err)
	}
	got, err := s.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if want := (Usage{PromptTokens: 110, CompletionTokens: 55, TotalTokens: 165}); got.Usage != want {
		t.Fatalf("两次用量应累加为 %+v，得到 %+v", want, got.Usage)
	}
}
