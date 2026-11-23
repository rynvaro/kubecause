package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestConversationSummaryOptimisticUpdate(t *testing.T) {
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
	// 新会话：摘要为空，游标为 0。
	got, err := s.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if got.Summary != "" || got.SummaryCursor != 0 {
		t.Fatalf("新会话摘要应为空、游标为 0，得到 %+v", got)
	}
	// 游标 0 时更新成功。
	ok, err := s.UpdateConversationSummary(ctx, c.ID, "摘要一", 10, 0)
	if err != nil || !ok {
		t.Fatalf("游标 0 更新应成功，得到 ok=%v err=%v", ok, err)
	}
	got, _ = s.GetConversation(ctx, c.ID)
	if got.Summary != "摘要一" || got.SummaryCursor != 10 {
		t.Fatalf("更新后应读到摘要一 / 游标 10，得到 %+v", got)
	}
	// 拿旧游标 0 再更新：应失败，且库里仍是“摘要一”，不能被覆盖、游标不能回退。
	ok, err = s.UpdateConversationSummary(ctx, c.ID, "摘要二", 20, 0)
	if err != nil {
		t.Fatalf("旧游标更新不应报错，只应返回 false: %v", err)
	}
	if ok {
		t.Fatalf("旧游标更新应返回 false")
	}
	got, _ = s.GetConversation(ctx, c.ID)
	if got.Summary != "摘要一" || got.SummaryCursor != 10 {
		t.Fatalf("旧游标更新失败后库里应仍是摘要一 / 游标 10，得到 %+v", got)
	}
	// 拿新游标 10 更新：成功。
	ok, err = s.UpdateConversationSummary(ctx, c.ID, "摘要二", 20, 10)
	if err != nil || !ok {
		t.Fatalf("游标 10 更新应成功，得到 ok=%v err=%v", ok, err)
	}
	got, _ = s.GetConversation(ctx, c.ID)
	if got.Summary != "摘要二" || got.SummaryCursor != 20 {
		t.Fatalf("更新后应读到摘要二 / 游标 20，得到 %+v", got)
	}
}

func TestMigrateAddsSummaryColumns(t *testing.T) {
	// 模拟 issue #19 之前的老库：conversations 表没有 summary /
	// summary_cursor，写一行老数据，再 Open——migrate 应自动补列，
	// 老数据不丢，摘要读出来是空、游标是 0。
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("建老库失败: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE conversations (
			id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL
		);
		INSERT INTO conversations(id, title, created_at, updated_at)
		VALUES('conv_old', '老会话', '2026-01-01', '2026-01-01');
	`)
	if err != nil {
		t.Fatalf("写老库失败: %v", err)
	}
	_ = raw.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	defer s.Close()
	got, err := s.GetConversation(ctx, "conv_old")
	if err != nil {
		t.Fatalf("读老会话失败: %v", err)
	}
	if got.Title != "老会话" || got.Summary != "" || got.SummaryCursor != 0 {
		t.Fatalf("老数据不应丢，摘要应为空、游标为 0，得到 %+v", got)
	}
	// 老库上更新摘要也能用。
	ok, err := s.UpdateConversationSummary(ctx, "conv_old", "摘要", 5, 0)
	if err != nil || !ok {
		t.Fatalf("老库更新摘要应成功，得到 ok=%v err=%v", ok, err)
	}
}
