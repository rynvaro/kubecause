// Package store 负责 SQLite 持久化。
//
// 设计要点：
//   - 单个 Go 进程里 API 和 Worker 都会写库，所以把连接池收敛到 1 个连接，
//     再配合 WAL 和 busy_timeout，避免 SQLite 出现 database is locked。
//     Slice 1 的量很小，串行写完全够用，比引入消息队列简单得多。
//   - 步骤和进度必须在同一个事务里提交：工具结果写进 run_steps 的同时，
//     runs 的 updated_at 一起更新。进程在半路崩掉时，要么这一步完整落盘，
//     要么完全没落盘，不会出现“结果丢了但状态却前进了”的中间态，
//     重启后才能安全地复用已提交步骤、不重复执行。
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 任务状态，见架构文档：queued -> running -> completed 或 failed。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// 审批 / 变更状态。
// Status 是审批状态：pending -> approved / rejected / expired。
// ExecutionStatus 是执行状态：not_started -> executing -> executed / failed / unknown / expired。
// VerificationStatus 是验证状态：not_started -> verifying -> passed / failed / timeout。
// 三者分开存，前端才能分别展示“写入成功了没有”和“验证通过了没有”，
// 写入成功但验证失败时不能混成一个“成功”。
const (
	ChangePending  = "pending"
	ChangeApproved = "approved"
	ChangeRejected = "rejected"
	ChangeExpired  = "expired"
)

const (
	ExecNotStarted = "not_started"
	ExecExecuting  = "executing"
	ExecExecuted   = "executed"
	ExecFailed     = "failed"
	ExecUnknown    = "unknown"
	ExecExpired    = "expired"
)

const (
	VerifyNotStarted = "not_started"
	VerifyVerifying  = "verifying"
	VerifyPassed     = "passed"
	VerifyFailed     = "failed"
	VerifyTimeout    = "timeout"
)

// Run 对应 runs 表，一行就是一次诊断任务。
type Run struct {
	ID             string `json:"id"`
	Namespace      string `json:"namespace"`
	Kind           string `json:"kind"` // Pod 或 Deployment
	Name           string `json:"name"`
	Question       string `json:"question"`
	Status         string `json:"status"`
	MaxSteps       int    `json:"maxSteps"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	// ReportJSON 是诊断报告的 JSON，空字符串表示还没有报告。
	ReportJSON string `json:"-"`
	// SuggestedJSON 是对话转正式诊断时带过来的修复建议（Slice 10），
	// 存 SuggestedRepair 的 JSON。Worker 看到它就直接拿建议去校验、
	// 生成提案，不用重新诊断一遍——以前重跑 8 步耗尽、置信度 low、
	// 没有提案，修复任务从来没成功过。空字符串表示没有带建议，
	// Worker 走正常的 Agent 诊断循环。
	SuggestedJSON string `json:"-"`
	// CreationJSON 是对话转正式诊断时带过来的创建建议（Slice 10），
	// 存 CreationSpec 的 JSON。Worker 看到它就直接校验、生成创建提案，
	// 同样不用重新诊断。空字符串表示不是创建任务。
	CreationJSON string    `json:"-"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// RunStep 对应 run_steps 表，序号在同一个任务内单调递增，
// 既是 Agent 的决策轨迹，也是前端时间线的唯一来源。
type RunStep struct {
	ID         int64     `json:"id"`
	RunID      string    `json:"runId"`
	Seq        int       `json:"seq"`
	Kind       string    `json:"kind"` // tool_call / tool_result / report / approval / error
	ToolName   string    `json:"toolName,omitempty"`
	CallID     string    `json:"callId,omitempty"`
	ArgsJSON   string    `json:"args,omitempty"`
	ResultJSON string    `json:"result,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"durationMs"`
	RetryCount int       `json:"retryCount"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ChangeRequest 对应 change_requests 表，一份不可变的变更提案。
// DeploymentUID 是提案生成时绑定的目标 Deployment UID，执行前要重新
// 核对，变了就说明目标已被重建，提案过期。ExecutionDetail /
// VerificationDetail 存执行和验证结果的 JSON，分开存、分开展示。
//
// Patches 是通用 Patch 列表（Slice 9）：一份提案可以改多个字段，
// 每个 Patch 是 op/path/value（目前只支持 op="replace"），
// 例如改镜像 /spec/template/spec/containers/0/image、改内存
// /spec/template/spec/containers/0/resources/limits/memory、
// 改副本数 /spec/replicas。OldValue / NewValue 只保留做展示用，
// 取第一个 Patch 的改前/改后值；执行时以 Patches 为准。
// 老提案没有 Patches（库里 patches_json 为空）时，读出来会从
// OldValue / NewValue 反推一条内存 limit 的 Patch，保证向后兼容。
type PatchOperation struct {
	Op    string `json:"op"`   // 目前只支持 "replace"
	Path  string `json:"path"` // 例如 /spec/template/spec/containers/0/image
	Value any    `json:"value"`
}

// CreationSpec 是一份创建 Deployment 的建议（Slice 10）。
// 只有创建才有这个结构：Patch 提案（kind="patch"）不填它。
// 字段是创建时必须由模型或用户给全的东西：
// 目标 namespace / Deployment 名 / 镜像 / 副本数，
// 再加可选的容器名和资源 requests/limits。
// 注意：只允许创建 Deployment，不直接创建裸 Pod——
// 裸 Pod 挂了不会自动重建、没有 rollout，
// 用户说“创建一个 Pod”时也要按 Deployment 给建议，
// 并在对话里说明原因。这是建议的形状定义，
// 能不能创建由 repair.ValidateCreation 再校验一遍。
// 本类型是 agent.SuggestedCreation 的规范定义（见 agent 包的别名），
// 和 PatchOperation 一样放在 store，避免循环引用。
type CreationSpec struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"` // Deployment 名
	Image         string `json:"image"`
	Replicas      int    `json:"replicas"`            // 0 表示未指定，校验时默认 1
	Container     string `json:"container,omitempty"` // 容器名，空时默认 = Name
	MemoryLimit   string `json:"memoryLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	CpuLimit      string `json:"cpuLimit,omitempty"`
	CpuRequest    string `json:"cpuRequest,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type ChangeRequest struct {
	ID            string           `json:"id"`
	RunID         string           `json:"runId"`
	Namespace     string           `json:"namespace"`
	Deployment    string           `json:"deployment"`
	Container     string           `json:"container"`
	OldValue      string           `json:"oldValue"`
	NewValue      string           `json:"newValue"`
	Patches       []PatchOperation `json:"patches,omitempty"`
	DeploymentUID string           `json:"deploymentUid,omitempty"`
	// Kind 是提案类型（Slice 10）："patch" 是改已有 Deployment 的字段，
	// "create" 是创建一个新的 Deployment。老提案没有这一列时读出来是 ""，
	// 按 "patch" 处理，保证向后兼容。
	Kind string `json:"kind,omitempty"`
	// Creation 是创建提案的完整规格（Slice 10），只有 kind="create" 时才有，
	// 库里以 creation_json 存，读出来反序列化。Patch 提案不填它。
	Creation           *CreationSpec `json:"creation,omitempty"`
	Status             string        `json:"status"`
	Approver           string        `json:"approver,omitempty"`
	ExecutionStatus    string        `json:"executionStatus"`
	VerificationStatus string        `json:"verificationStatus"`
	ExecutionDetail    string        `json:"executionDetail,omitempty"`
	VerificationDetail string        `json:"verificationDetail,omitempty"`
	CreatedAt          time.Time     `json:"createdAt"`
	UpdatedAt          time.Time     `json:"updatedAt"`
}

// Conversation 对应 conversations 表，一次对话会话。
// 对话和单次诊断是两条线：对话只读、不直接改集群，
// 需要修复时转成 /api/runs 的正式诊断再走审批。
type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ConversationMessage 对应 messages 表，一条会话消息。
// Role 是 user / assistant / tool；ToolCalls 存本轮调过的工具
// （JSON），Structured 存结构化结果（JSON，前端渲染表格/卡片用）。
type ConversationMessage struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversationId"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	ToolCalls      string    `json:"toolCalls,omitempty"`
	Structured     string    `json:"structured,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Store 是 SQLite 的唯一入口。
type Store struct {
	db *sql.DB
}

// NewID 生成 run_ 开头的随机 ID，碰撞概率可忽略，演示和面试都够用。
func NewID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Open 打开数据库并执行迁移。迁移用 CREATE TABLE IF NOT EXISTS，
// 启动时自动执行，老库升级也不会丢数据。
func Open(path string) (*Store, error) {
	// _pragma 用 WAL + busy_timeout：读和写可以并行，写冲突时等 5 秒再报错，
	// 而不是立刻报 database is locked。这是单文件 SQLite 在 API+Worker
	// 同进程读写时的标准做法。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	// 只留一个连接：SQLite 的写本身就是串行的，池子大了反而会互相锁。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 返回底层数据库连接，供知识库 FTS5 索引使用。
// 只读索引的建表和查询走这个连接，业务写库仍走 Store 的方法，
// 避免外部代码绕过 Store 直接写 runs / run_steps。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS runs (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			question TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			max_steps INTEGER NOT NULL DEFAULT 8,
			timeout_seconds INTEGER NOT NULL DEFAULT 60,
			report_json TEXT NOT NULL DEFAULT '',
			suggested_json TEXT NOT NULL DEFAULT '',
			creation_json TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS run_steps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES runs(id),
			seq INTEGER NOT NULL,
			kind TEXT NOT NULL,
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_run_steps_run_seq ON run_steps(run_id, seq)`,
		`CREATE TABLE IF NOT EXISTS change_requests (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(id),
			namespace TEXT NOT NULL,
			deployment TEXT NOT NULL,
			container TEXT NOT NULL,
			old_value TEXT NOT NULL,
			new_value TEXT NOT NULL,
			patches_json TEXT NOT NULL DEFAULT '',
			deployment_uid TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'patch',
			creation_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			approver TEXT NOT NULL DEFAULT '',
			execution_status TEXT NOT NULL DEFAULT 'not_started',
			verification_status TEXT NOT NULL DEFAULT 'not_started',
			execution_detail TEXT NOT NULL DEFAULT '',
			verification_detail TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			decided_at DATETIME
		)`,
		// 对话会话（Slice 4）：conversations 是一次会话，
		// messages 是会话里的消息，role 分 user / assistant / tool。
		// 历史只保留最近的消息给模型（见 chat 包的截断策略），
		// 数据库里全量保留，前端翻历史时能看到完整上下文。
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			conversation_id TEXT NOT NULL REFERENCES conversations(id),
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			structured TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_conv ON messages(conversation_id, id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("迁移失败: %w", err)
		}
	}
	// 老库升级：Slice 1/2 建的库没有这三列，启动时补上，不丢数据。
	// SQLite 的 ADD COLUMN 没有 IF NOT EXISTS，所以先查表结构再决定加不加。
	for _, col := range []struct{ name, typ string }{
		{"deployment_uid", "TEXT NOT NULL DEFAULT ''"},
		{"patches_json", "TEXT NOT NULL DEFAULT ''"},
		{"execution_detail", "TEXT NOT NULL DEFAULT ''"},
		{"verification_detail", "TEXT NOT NULL DEFAULT ''"},
		{"kind", "TEXT NOT NULL DEFAULT 'patch'"},
		{"creation_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, "change_requests", col.name, col.typ); err != nil {
			return err
		}
	}
	// runs 表的老库升级（Slice 10）：对话带过来的建议存在这两列，
	// 老库没有时补上，空字符串表示没有带建议。
	for _, col := range []struct{ name, typ string }{
		{"suggested_json", "TEXT NOT NULL DEFAULT ''"},
		{"creation_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, "runs", col.name, col.typ); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn 在列不存在时才 ALTER TABLE 加列，列已存在时什么都不做。
func (s *Store) ensureColumn(ctx context.Context, table, column, typ string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("检查表结构失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, typ)); err != nil {
		return fmt.Errorf("加列 %s.%s 失败: %w", table, column, err)
	}
	return nil
}

func now() time.Time { return time.Now().UTC() }

// DefaultRunMaxSteps / DefaultRunTimeoutSeconds 是创建任务时
// MaxSteps / TimeoutSeconds 没填时的默认值：8 步 / 60 秒。
// 不再是写死的常量：配置中心（internal/config）的 run.maxSteps /
// run.timeoutSeconds 在启动和热更新时由 ApplyRunDefaults 填进来。
// /api/runs 请求体里显式传了 maxSteps / timeoutSeconds 时，
// 请求体的值优先，配置只决定“没填时用什么”。
var DefaultRunMaxSteps = 8

// DefaultRunTimeoutSeconds 是创建任务时 TimeoutSeconds 没填时的默认值（秒）。
var DefaultRunTimeoutSeconds = 60

// ApplyRunDefaults 把配置中心的值应用到任务创建默认值：热加载时调它。
// 非法值不改，保持旧值。
func ApplyRunDefaults(maxSteps, timeoutSeconds int) {
	if maxSteps >= 1 && maxSteps <= 50 {
		DefaultRunMaxSteps = maxSteps
	}
	if timeoutSeconds >= 5 && timeoutSeconds <= 600 {
		DefaultRunTimeoutSeconds = timeoutSeconds
	}
}

// CreateRun 创建一个 queued 任务。
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	r.ID = NewID("run_")
	r.Status = StatusQueued
	if r.MaxSteps <= 0 {
		r.MaxSteps = DefaultRunMaxSteps
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = DefaultRunTimeoutSeconds
	}
	r.CreatedAt = now()
	r.UpdatedAt = r.CreatedAt
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id, namespace, kind, name, question, status, max_steps, timeout_seconds, report_json, suggested_json, creation_json, error, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Namespace, r.Kind, r.Name, r.Question, r.Status, r.MaxSteps, r.TimeoutSeconds, "", r.SuggestedJSON, r.CreationJSON, "", r.CreatedAt, r.UpdatedAt)
	return err
}

// GetRun 按 ID 取任务，不存在返回 sql.ErrNoRows。
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, namespace, kind, name, question, status, max_steps, timeout_seconds, report_json, suggested_json, creation_json, error, created_at, updated_at FROM runs WHERE id=?`, id)
	r := &Run{}
	err := row.Scan(&r.ID, &r.Namespace, &r.Kind, &r.Name, &r.Question, &r.Status, &r.MaxSteps, &r.TimeoutSeconds, &r.ReportJSON, &r.SuggestedJSON, &r.CreationJSON, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ClaimNext 领取一个 queued 任务并原子地改成 running。
// 用一行 UPDATE ... WHERE status='queued' 保证同一时刻只有一个 Worker
// 能领到同一个任务，不需要额外的分布式锁。返回 nil, nil 表示暂时没有任务。
func (s *Store) ClaimNext(ctx context.Context) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx,
		`SELECT id, namespace, kind, name, question, status, max_steps, timeout_seconds, report_json, suggested_json, creation_json, error, created_at, updated_at
		 FROM runs WHERE status='queued' ORDER BY created_at ASC LIMIT 1`)
	r := &Run{}
	if err := row.Scan(&r.ID, &r.Namespace, &r.Kind, &r.Name, &r.Question, &r.Status, &r.MaxSteps, &r.TimeoutSeconds, &r.ReportJSON, &r.SuggestedJSON, &r.CreationJSON, &r.Error, &r.CreatedAt, &r.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status='running', updated_at=? WHERE id=? AND status='queued'`, now(), r.ID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 被别人抢先领走了，本次不算领到。
		return nil, nil
	}
	r.Status = StatusRunning
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// ResetRunningToQueued 是重启恢复的关键一步：进程崩掉时 status 停在 running
// 的任务，步骤已经落盘的部分要保留，状态改回 queued，让 Worker 重新领走，
// Agent 再根据已提交步骤续跑，而不是从头重做。
func (s *Store) ResetRunningToQueued(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status='queued', updated_at=? WHERE status='running'`, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetStatus 更新任务状态，可选同时写入报告 JSON 和错误信息。
func (s *Store) SetStatus(ctx context.Context, id, status, reportJSON, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, report_json=?, error=?, updated_at=? WHERE id=?`,
		status, reportJSON, errMsg, now(), id)
	return err
}

// NextSeq 返回该任务下一个步骤序号（已有最大序号 +1）。
func (s *Store) NextSeq(ctx context.Context, runID string) (int, error) {
	var maxSeq sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM run_steps WHERE run_id=?`, runID).Scan(&maxSeq)
	if err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

// AppendStep 在同一个事务里写一步并更新任务的 updated_at。
// 调用方负责先算好 Seq；UNIQUE(run_id, seq) 是最后一道防线，
// 防止并发或重试把同一序号写两遍。
func (s *Store) AppendStep(ctx context.Context, st *RunStep) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	st.CreatedAt = now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO run_steps(run_id, seq, kind, tool_name, call_id, args_json, result_json, summary, error, duration_ms, retry_count, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		st.RunID, st.Seq, st.Kind, st.ToolName, st.CallID, st.ArgsJSON, st.ResultJSON, st.Summary, st.Error, st.DurationMs, st.RetryCount, st.CreatedAt)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=? WHERE id=?`, now(), st.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSteps 按序号返回一个任务的全部步骤，即时间线。
func (s *Store) ListSteps(ctx context.Context, runID string) ([]RunStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, seq, kind, tool_name, call_id, args_json, result_json, summary, error, duration_ms, retry_count, created_at
		 FROM run_steps WHERE run_id=? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunStep
	for rows.Next() {
		var st RunStep
		if err := rows.Scan(&st.ID, &st.RunID, &st.Seq, &st.Kind, &st.ToolName, &st.CallID, &st.ArgsJSON, &st.ResultJSON, &st.Summary, &st.Error, &st.DurationMs, &st.RetryCount, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListStepsAfter 返回序号大于 after 的步骤，供前端增量轮询。
func (s *Store) ListStepsAfter(ctx context.Context, runID string, after int) ([]RunStep, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, seq, kind, tool_name, call_id, args_json, result_json, summary, error, duration_ms, retry_count, created_at
		 FROM run_steps WHERE run_id=? AND seq>? ORDER BY seq ASC`, runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunStep
	for rows.Next() {
		var st RunStep
		if err := rows.Scan(&st.ID, &st.RunID, &st.Seq, &st.Kind, &st.ToolName, &st.CallID, &st.ArgsJSON, &st.ResultJSON, &st.Summary, &st.Error, &st.DurationMs, &st.RetryCount, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// marshalPatches 把 Patch 列表序列化成存库的 JSON，空列表存 ""。
func marshalPatches(patches []PatchOperation) string {
	if len(patches) == 0 {
		return ""
	}
	b, _ := json.Marshal(patches)
	return string(b)
}

// unmarshalPatches 把库里的 patches_json 读回 Patch 列表。
// 空字符串表示老提案（Slice 8 及以前建的库没有这一列），
// 此时从 oldValue / newValue 反推一条内存 limit 的 Patch，
// 让老提案读出来也是通用 Patch 格式，前端和执行器不用分两套逻辑。
// 反推的 path 用容器下标 0：执行前 repair 层会按容器名重算下标，
// 这里只是给展示和兼容用，不直接拿去 Patch。
func unmarshalPatches(patchesJSON, oldValue, newValue string) []PatchOperation {
	if strings.TrimSpace(patchesJSON) != "" {
		var out []PatchOperation
		if err := json.Unmarshal([]byte(patchesJSON), &out); err == nil && len(out) > 0 {
			return out
		}
	}
	if oldValue != "" && newValue != "" {
		return []PatchOperation{{
			Op: "replace", Path: "/spec/template/spec/containers/0/resources/limits/memory", Value: newValue,
		}}
	}
	return nil
}

// marshalCreation 把创建规格序列化成存库的 JSON，空（不是创建提案）存 ""。
func marshalCreation(c *CreationSpec) string {
	if c == nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// unmarshalCreation 把库里的 creation_json 读回创建规格。
// 空字符串表示 Patch 提案（或 Slice 10 之前的老提案），返回 nil。
func unmarshalCreation(creationJSON string) *CreationSpec {
	if strings.TrimSpace(creationJSON) == "" {
		return nil
	}
	var out CreationSpec
	if err := json.Unmarshal([]byte(creationJSON), &out); err != nil {
		return nil
	}
	return &out
}

// CreateChangeRequest 创建一份待审批提案，同一任务重复创建时直接返回已有的那份，
// 保证一个任务最多只有一份有效提案，不会因为 Worker 重试而重复出提案。
func (s *Store) CreateChangeRequest(ctx context.Context, c *ChangeRequest) (*ChangeRequest, error) {
	existing, err := s.GetChangeRequestByRun(ctx, c.RunID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	c.ID = NewID("chg_")
	c.Status = ChangePending
	c.ExecutionStatus = ExecNotStarted
	c.VerificationStatus = VerifyNotStarted
	if c.Kind == "" {
		c.Kind = "patch"
	}
	c.CreatedAt = now()
	c.UpdatedAt = c.CreatedAt
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO change_requests(id, run_id, namespace, deployment, container, old_value, new_value, patches_json, deployment_uid, kind, creation_json, status, approver, execution_status, verification_status, execution_detail, verification_detail, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.RunID, c.Namespace, c.Deployment, c.Container, c.OldValue, c.NewValue, marshalPatches(c.Patches), c.DeploymentUID, c.Kind, marshalCreation(c.Creation), c.Status, "", c.ExecutionStatus, c.VerificationStatus, "", "", c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// scanChangeRequest 把一行 change_requests 读进结构体，
// patches_json 和 old/new 值的映射集中在这里，三个读入口共用。
func scanChangeRequest(row *sql.Row) (*ChangeRequest, error) {
	c := &ChangeRequest{}
	var patchesJSON string
	var creationJSON string
	err := row.Scan(&c.ID, &c.RunID, &c.Namespace, &c.Deployment, &c.Container, &c.OldValue, &c.NewValue, &patchesJSON, &c.DeploymentUID, &c.Kind, &creationJSON, &c.Status, &c.Approver, &c.ExecutionStatus, &c.VerificationStatus, &c.ExecutionDetail, &c.VerificationDetail, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Patches = unmarshalPatches(patchesJSON, c.OldValue, c.NewValue)
	c.Creation = unmarshalCreation(creationJSON)
	return c, nil
}

// GetChangeRequestByRun 取一个任务的提案。
func (s *Store) GetChangeRequestByRun(ctx context.Context, runID string) (*ChangeRequest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_id, namespace, deployment, container, old_value, new_value, patches_json, deployment_uid, kind, creation_json, status, approver, execution_status, verification_status, execution_detail, verification_detail, created_at, updated_at
		 FROM change_requests WHERE run_id=? ORDER BY created_at ASC LIMIT 1`, runID)
	return scanChangeRequest(row)
}

// GetChangeRequest 按提案 ID 取。
func (s *Store) GetChangeRequest(ctx context.Context, id string) (*ChangeRequest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, run_id, namespace, deployment, container, old_value, new_value, patches_json, deployment_uid, kind, creation_json, status, approver, execution_status, verification_status, execution_detail, verification_detail, created_at, updated_at
		 FROM change_requests WHERE id=?`, id)
	return scanChangeRequest(row)
}

// DecideApproval 是审批决定的唯一入口，幂等：
//   - pending 时改成 approved / rejected，记录审批人和决定时间；
//   - 已经决定过时直接返回当前结果，不重复改状态。
//
// 这点很重要：前端重复点两次、网络重试、Worker 重启都不能让一份提案
// 被批准两次，也不能让已拒绝的提案又变回待审批。
func (s *Store) DecideApproval(ctx context.Context, id, decision, approver string) (*ChangeRequest, bool, error) {
	c, err := s.GetChangeRequest(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if c.Status != ChangePending {
		return c, true, nil // alreadyDecided=true
	}
	if decision != ChangeApproved && decision != ChangeRejected {
		return nil, false, fmt.Errorf("decision 只能是 approved 或 rejected")
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE change_requests SET status=?, approver=?, decided_at=?, updated_at=? WHERE id=? AND status='pending'`,
		decision, approver, now(), now(), id)
	if err != nil {
		return nil, false, err
	}
	c.Status = decision
	c.Approver = approver
	return c, false, nil
}

// ClaimApprovedChange 原子领取一份已批准、未执行的提案，并把它标为 executing。
// 用一行 UPDATE ... WHERE status='approved' AND execution_status='not_started'
// 保证同一时刻只有一个 Worker 能领到同一份提案。返回 nil, nil 表示暂时没有。
// 领取和执行在同一个 Worker 循环里串行做，不需要分布式锁。
func (s *Store) ClaimApprovedChange(ctx context.Context) (*ChangeRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx,
		`SELECT id, run_id, namespace, deployment, container, old_value, new_value, patches_json, deployment_uid, kind, creation_json, status, approver, execution_status, verification_status, execution_detail, verification_detail, created_at, updated_at
		 FROM change_requests WHERE status='approved' AND execution_status='not_started' ORDER BY created_at ASC LIMIT 1`)
	c, err := scanChangeRequest(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE change_requests SET execution_status=?, updated_at=? WHERE id=? AND status='approved' AND execution_status='not_started'`, ExecExecuting, now(), c.ID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	c.ExecutionStatus = ExecExecuting
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

// SetExecution 更新一份提案的执行状态和执行结果 JSON（分开存，执行和验证不混）。
func (s *Store) SetExecution(ctx context.Context, id, executionStatus, detailJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE change_requests SET execution_status=?, execution_detail=?, updated_at=? WHERE id=?`,
		executionStatus, detailJSON, now(), id)
	return err
}

// SetVerification 更新一份提案的验证状态和验证结果 JSON。
func (s *Store) SetVerification(ctx context.Context, id, verificationStatus, detailJSON string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE change_requests SET verification_status=?, verification_detail=?, updated_at=? WHERE id=?`,
		verificationStatus, detailJSON, now(), id)
	return err
}

// SetChangeExpired 把提案标为过期：审批状态和执行状态都置为 expired，
// 并把原因写进执行结果里。过期是终态，Worker 不会再领取它。
func (s *Store) SetChangeExpired(ctx context.Context, id, reason string) error {
	detail := `{"status":"expired","error":` + quoteJSON(reason) + `}`
	_, err := s.db.ExecContext(ctx,
		`UPDATE change_requests SET status=?, execution_status=?, execution_detail=?, updated_at=? WHERE id=?`,
		ChangeExpired, ExecExpired, detail, now(), id)
	return err
}

// ResetExecutingChanges 是重启恢复的一部分：进程在执行中途崩掉时，
// execution_status 停在 executing 的提案要改回 approved + not_started，
// 让 Worker 重新领取。重跑是安全的，因为执行前校验会先看当前值：
// 如果 Patch 其实已经生效了（当前值等于新值），会直接记为已执行，
// 不会重复 Patch；如果还没生效，就重新走一遍 dry-run + Patch。
func (s *Store) ResetExecutingChanges(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE change_requests SET execution_status=?, updated_at=? WHERE execution_status=?`,
		ExecNotStarted, now(), ExecExecuting)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// 对话会话（Slice 4）
// ---------------------------------------------------------------------------

// CreateConversation 创建一个会话，标题默认取第一条用户消息的前 20 个字，
// 创建时还没有消息，标题先为空，首条消息写入时再补。
func (s *Store) CreateConversation(ctx context.Context, title string) (*Conversation, error) {
	c := &Conversation{ID: NewID("conv_"), Title: title, CreatedAt: now(), UpdatedAt: now()}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, title, created_at, updated_at) VALUES(?,?,?,?)`,
		c.ID, c.Title, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// GetConversation 按 ID 取会话。
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, title, created_at, updated_at FROM conversations WHERE id=?`, id)
	c := &Conversation{}
	if err := row.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	return c, nil
}

// ListConversations 按更新时间倒序列出会话。
func (s *Store) ListConversations(ctx context.Context) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, created_at, updated_at FROM conversations ORDER BY updated_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendMessage 往会话里追加一条消息，并更新会话的 updated_at。
// 首条用户消息写入时，若会话标题为空，用它做标题（截断到 20 个字）。
func (s *Store) AppendMessage(ctx context.Context, m *ConversationMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	m.CreatedAt = now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages(conversation_id, role, content, tool_calls, structured, created_at) VALUES(?,?,?,?,?,?)`,
		m.ConversationID, m.Role, m.Content, m.ToolCalls, m.Structured, m.CreatedAt)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	m.ID = id
	if m.Role == "user" {
		_, _ = tx.ExecContext(ctx,
			`UPDATE conversations SET title=COALESCE(NULLIF(title,''),?), updated_at=? WHERE id=?`,
			truncateTitle(m.Content), now(), m.ConversationID)
	} else {
		_, _ = tx.ExecContext(ctx, `UPDATE conversations SET updated_at=? WHERE id=?`, now(), m.ConversationID)
	}
	return tx.Commit()
}

// ListMessages 按时间顺序返回一个会话的全部消息。
// 调用方负责截断：给模型时只取最近 10 条（见 chat 包），
// 给前端时全量返回，前端能看到完整历史。
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]ConversationMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conversation_id, role, content, tool_calls, structured, created_at FROM messages WHERE conversation_id=? ORDER BY id ASC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationMessage{}
	for rows.Next() {
		var m ConversationMessage
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.ToolCalls, &m.Structured, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= 20 {
		return s
	}
	return string(r[:20])
}
