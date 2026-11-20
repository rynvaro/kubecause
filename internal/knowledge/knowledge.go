// Package knowledge 提供排障手册检索。
//
// Slice 1 用内存里的 Markdown 片段做关键词检索，不引入向量数据库。
// 原因：首批只有 OOM、镜像拉取失败、探针失败三类，语料很小，
// 关键词加别名已经够准，而且结果确定、可解释，面试时好讲。
//
// Slice 2 把同样的 Markdown 建成 SQLite FTS5 索引。
// 为什么用 FTS5 而不是向量库：语料是几篇手册，FTS5 是 SQLite 自带的
// 全文检索，不用额外服务，启动时建索引就行，查询按相关性排序，
// 还能返回来源文件。接口 Search 不变，FTS5 为空或查不到时
// 回退到 Slice 1 的关键词检索，保证检索永远有结果可回。
package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Snippet 是一条带来源的检索结果。
type Snippet struct {
	Title   string `json:"title"`
	Source  string `json:"source"`
	Content string `json:"content"`
}

// 手册条目：关键词和别名用于匹配，内容是给模型看的判断要点。
type entry struct {
	title    string
	source   string
	content  string
	keywords []string
}

var entries = []entry{
	{
		title:   "OOMKilled / 容器内存不足",
		source:  "runbooks/oom.md",
		content: "容器因内存超限被内核杀掉，退出码 137，Reason 为 OOMKilled，LastState.Terminated 有记录。先确认 Deployment 的 resources.limits.memory 是否过低，再看应用是否有内存泄漏。临时缓解是调高 limit，根因仍需查应用内存曲线。",
		keywords: []string{"oomkilled", "oom", "out of memory", "137", "内存", "重启", "restart"},
	},
	{
		title:   "镜像拉取失败 ImagePullBackOff",
		source:  "runbooks/imagepull.md",
		content: "Pod 卡在 ImagePullBackOff，通常是镜像名或 tag 写错、镜像仓库不可达、或 imagePullSecrets 缺失。先查 Events 里的 Failed to pull image，再核对 Deployment 的 image 字段和 Secret。",
		keywords: []string{"imagepullbackoff", "imagepull", "镜像", "pull", "ErrImagePull", "Failed to pull"},
	},
	{
		title:   "探针失败 / Liveness / Readiness",
		source:  "runbooks/probe.md",
		content: "Liveness 探针失败会导致容器被重启，Readiness 失败会让 Pod 脱离 Service。先看探针的路径、端口和超时，再看应用启动耗时是否超过 initialDelaySeconds，日志里是否有启动失败。",
		keywords: []string{"probe", "liveness", "readiness", "探针", "健康检查", "unhealthy"},
	},
	{
		title:   "CrashLoopBackOff / 容器反复崩溃重启",
		source:  "runbooks/crashloop.md",
		content: "Pod 处于 CrashLoopBackOff，容器启动后很快退出又被拉起。先看上次终止的原因和退出码：OOMKilled 是内存问题，Error 退出码 1 常是应用启动失败。再看日志最后几行，应用 panic、配置报错都会让容器一启动就退出。",
		keywords: []string{"crashloopbackoff", "crashloop", "崩溃", "反复重启", "backoff", "退出码"},
	},
	{
		title:   "Pending / Pod 调度失败",
		source:  "runbooks/pending.md",
		content: "Pod 长时间处于 Pending，Events 里出现 FailedScheduling。先看原因：节点资源不足、节点选择器没有节点满足、污点没有对应容忍、PersistentVolumeClaim 没绑定。",
		keywords: []string{"pending", "failedscheduling", "调度", "调度失败", "资源不足", "taint", "污点"},
	},
	{
		title:   "Service 无 Endpoints / 服务访问不到",
		source:  "runbooks/service-endpoints.md",
		content: "Service 存在但 Endpoints 为空，访问没有后端响应。先核对 Service 的 selector 和 Pod 的 labels 是否对得上，再看 Pod 的 Readiness 探针是否通过，最后看 Service 端口和容器端口是否一致。",
		keywords: []string{"endpoints", "service", "访问不到", "selector", "标签", "无后端"},
	},
	{
		title:   "ConfigMap / Secret 缺失",
		source:  "runbooks/configmap-secret.md",
		content: "Pod 启动失败，Events 里出现 FailedMount 或 CreateContainerConfigError，日志报 config file not found。先看 Deployment 挂载的 ConfigMap / Secret 在 namespace 里存不存在，再看 key 名有没有写错。",
		keywords: []string{"configmap", "secret", "failedmount", "createcontainerconfigerror", "配置", "缺失"},
	},
	{
		title:   "资源配额不足 / ResourceQuota",
		source:  "runbooks/quota.md",
		content: "Deployment 扩容或新建 Pod 时被拒绝，Events 里出现 exceeded quota。先看 namespace 的 ResourceQuota 和 LimitRange，再看是 CPU、内存、Pod 数量还是其他资源超了。",
		keywords: []string{"quota", "resourcequota", "配额", "exceeded quota", "超限"},
	},
}

// Search 按关键词和别名返回最多 3 条相关片段。
// 匹配不区分大小写，query 为空返回空结果，不返回全部手册。
// 这是 Slice 1 的回退实现，FTS5 查不到时仍用它。
func Search(query string) []Snippet {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var out []Snippet
	for _, e := range entries {
		for _, kw := range e.keywords {
			if strings.Contains(q, strings.ToLower(kw)) || strings.Contains(strings.ToLower(kw), q) {
				out = append(out, Snippet{Title: e.title, Source: e.source, Content: e.content})
				break
			}
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// FTS5 索引（Slice 2）
// ---------------------------------------------------------------------------

// Index 持有 FTS5 索引的数据库连接。
// Enabled 表示索引已建成且至少有一篇文档，
// 为 false 时调用方应回退到 Search 关键词检索。
type Index struct {
	DB      *sql.DB
	Enabled bool
}

// NewIndex 创建索引对象，不建表，建表由 Build 完成。
func NewIndex(db *sql.DB) *Index { return &Index{DB: db} }

// Build 从 dir 下的 *.md 文件建 FTS5 索引。
// 表是 runbook_fts，字段 title / source / content / keywords，
// 分词用 unicode61。
// 为什么要多一个 keywords 列：unicode61 会把一整句中文当成一个长词，
// 例如“容器因内存超限被内核杀掉”是一个词，查“内存”就查不到。
// keywords 列存的是用空格分开的别名（OOM、内存、探针……），
// 每个别名都是独立的词，短词查询才能命中；英文长词
// （OOMKilled、ImagePullBackOff）本来就能在 content 里命中，
// 两边互补。每次 Build 先清空再重建，启动时跑一次即可，
// 手册改了重启就生效。
func (idx *Index) Build(ctx context.Context, dir string) error {
	if idx.DB == nil {
		return fmt.Errorf("FTS5 索引缺少数据库连接")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("目录 %s 下没有找到 runbooks/*.md", dir)
	}
	// 先删旧表再建：老库可能是三列的表，直接建表会保留旧结构，
	// 插入四列时报错。FTS 索引是派生数据，删了重建没有损失。
	if _, err := idx.DB.ExecContext(ctx, `DROP TABLE IF EXISTS runbook_fts`); err != nil {
		return fmt.Errorf("清理旧 FTS5 表失败: %w", err)
	}
	if _, err := idx.DB.ExecContext(ctx, `CREATE VIRTUAL TABLE runbook_fts USING fts5(title, source, content, keywords, tokenize='unicode61')`); err != nil {
		return fmt.Errorf("创建 FTS5 表失败: %w", err)
	}
	keywordsBySource := map[string]string{}
	for _, e := range entries {
		keywordsBySource[e.source] = strings.Join(e.keywords, " ")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		content := string(b)
		title := titleFromMarkdown(content)
		if title == "" {
			title = filepath.Base(f)
		}
		source := "runbooks/" + filepath.Base(f)
		keywords := keywordsBySource[source]
		if _, err := idx.DB.ExecContext(ctx, `INSERT INTO runbook_fts(title, source, content, keywords) VALUES(?,?,?,?)`, title, source, content, keywords); err != nil {
			return fmt.Errorf("写入 FTS5 索引失败: %w", err)
		}
	}
	idx.Enabled = true
	return nil
}

// Search 先用 FTS5 按相关性查，查不到或出错时返回空和错误，
// 调用方回退到 Search 关键词检索。query 为空直接返回空。
func (idx *Index) Search(ctx context.Context, query string) ([]Snippet, error) {
	if idx == nil || idx.DB == nil || !idx.Enabled {
		return nil, fmt.Errorf("FTS5 索引未启用")
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, nil
	}
	match := buildMatchQuery(q)
	if match == "" {
		return nil, nil
	}
	rows, err := idx.DB.QueryContext(ctx, `SELECT title, source, content FROM runbook_fts WHERE runbook_fts MATCH ? ORDER BY rank LIMIT 3`, match)
	if err != nil {
		return nil, fmt.Errorf("FTS5 查询失败: %w", err)
	}
	defer rows.Close()
	var out []Snippet
	for rows.Next() {
		var s Snippet
		if err := rows.Scan(&s.Title, &s.Source, &s.Content); err != nil {
			return nil, err
		}
		// content 是整篇手册，给模型看要截短，只保留前 600 字，来源保留。
		s.Content = truncateRunes(s.Content, 600)
		out = append(out, s)
	}
	return out, rows.Err()
}

func titleFromMarkdown(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// buildMatchQuery 把用户查询转成 FTS5 的 MATCH 表达式。
// 做法：只保留字母、数字和中文字符组成的词，
// 每个词用双引号加前缀通配符 `"词"*`，词之间用 OR 连接。
// 前缀通配符是必要的：unicode61 会把英文长词和中文长句当成一个词，
// `"探针"*` 才能命中 keywords 列里的“探针”，
// `"OOMKilled"*` 才能命中 content 里的 OOMKilled。
// 中文长词（例如“内存不足”，4 个字）还要再切成二字词
// （内存、存不足、不足），否则 keywords 列里只有“内存”
// 这一个词，整词查不到。特殊字符直接丢掉，不会让 FTS5 语法报错。
func buildMatchQuery(query string) string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, string(cur))
			cur = nil
		}
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	if len(tokens) == 0 {
		return ""
	}
	if len(tokens) > 6 {
		tokens = tokens[:6]
	}
	seen := map[string]bool{}
	var quoted []string
	add := func(t string) {
		t = strings.ReplaceAll(t, `"`, "")
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		quoted = append(quoted, `"`+t+`"*`)
	}
	for _, t := range tokens {
		add(t)
		// 中文长词切二字词，让“内存不足”能命中 keywords 里的“内存”。
		r := []rune(t)
		if len(r) >= 3 {
			isCJK := true
			for _, ch := range r {
				if ch < 0x4E00 || ch > 0x9FFF {
					isCJK = false
					break
				}
			}
			if isCJK {
				for i := 0; i+2 <= len(r); i++ {
					add(string(r[i : i+2]))
				}
			}
		}
	}
	return strings.Join(quoted, " OR ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
