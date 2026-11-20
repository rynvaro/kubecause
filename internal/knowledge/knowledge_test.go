package knowledge

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFTSBuildAndSearch(t *testing.T) {
	db := openMemoryDB(t)
	idx := NewIndex(db)
	// 测试从 internal/knowledge 跑，runbooks 在 ../../runbooks。
	if err := idx.Build(context.Background(), "../../runbooks"); err != nil {
		t.Fatalf("建 FTS5 索引失败: %v", err)
	}
	if !idx.Enabled {
		t.Fatalf("建完索引后 Enabled 应为 true")
	}
	cases := []struct {
		query string
		want  string // 期望命中片段的来源文件
	}{
		{"OOMKilled", "runbooks/oom.md"},
		{"ImagePullBackOff", "runbooks/imagepull.md"},
		{"探针", "runbooks/probe.md"},
		{"内存不足", "runbooks/oom.md"},
		{"Liveness probe failed", "runbooks/probe.md"},
	}
	for _, tc := range cases {
		got, err := idx.Search(context.Background(), tc.query)
		if err != nil {
			t.Fatalf("查询 %q 失败: %v", tc.query, err)
		}
		if len(got) == 0 {
			t.Fatalf("查询 %q 应命中，实际为空", tc.query)
		}
		found := false
		for _, s := range got {
			if s.Source == tc.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("查询 %q 应命中 %s，实际 %+v", tc.query, tc.want, got)
		}
	}
}

func TestFTSNotEnabledReturnsError(t *testing.T) {
	db := openMemoryDB(t)
	idx := NewIndex(db)
	if _, err := idx.Search(context.Background(), "OOMKilled"); err == nil {
		t.Fatalf("未建索引时 Search 应返回错误，让调用方回退到关键词检索")
	}
}

func TestFTSBuildMissingDir(t *testing.T) {
	db := openMemoryDB(t)
	idx := NewIndex(db)
	if err := idx.Build(context.Background(), "../../runbooks-not-exist"); err == nil {
		t.Fatalf("目录不存在时 Build 应报错")
	}
}

func TestKeywordFallbackStillWorks(t *testing.T) {
	// FTS5 查不到或未启用时，Search 关键词检索仍是兜底。
	if got := Search("OOMKilled"); len(got) == 0 || got[0].Source != "runbooks/oom.md" {
		t.Fatalf("关键词检索应命中 OOM 手册，得到 %+v", got)
	}
	if got := Search("ImagePullBackOff"); len(got) == 0 {
		t.Fatalf("关键词检索应命中镜像手册")
	}
	if got := Search(""); got != nil {
		t.Fatalf("空查询应返回空，不返回全部手册")
	}
}

func TestBuildMatchQuery(t *testing.T) {
	if q := buildMatchQuery("Liveness probe failed"); q == "" || len(q) < 5 {
		t.Fatalf("MATCH 查询不应为空: %q", q)
	}
	if q := buildMatchQuery(""); q != "" {
		t.Fatalf("空查询应返回空 MATCH: %q", q)
	}
}
