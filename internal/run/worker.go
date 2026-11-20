package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/model"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// Worker 是后台任务执行器：轮询领取 queued 任务，跑 Agent 循环，
// 写报告，必要时生成一份待审批的修复提案。
// 单个 Go 进程同时跑 HTTP API 和 Worker，关闭浏览器页面后任务继续执行，
// 因为任务状态在 SQLite 里，不在内存里。
type Worker struct {
	Store *store.Store
	Tools agent.Tools
	Model agent.Model
	// PollInterval 是轮询间隔，Slice 1 用 500ms，演示时几乎立刻开始。
	PollInterval time.Duration
}

// NewWorker 组装 Worker。Model 由调用方按 MODEL_MODE 传入。
func NewWorker(s *store.Store, tools agent.Tools, m agent.Model) *Worker {
	return &Worker{Store: s, Tools: tools, Model: m, PollInterval: 500 * time.Millisecond}
}

// StartupRecovery 在进程启动时调用：把上次崩掉时停在 running 的任务
// 改回 queued，步骤保留，Agent 会根据已提交步骤续跑。
// 这是“重启恢复不重复执行”的第一步，第二步在 Agent.Run 里做指纹去重。
func (w *Worker) StartupRecovery(ctx context.Context) (int64, error) {
	return w.Store.ResetRunningToQueued(ctx)
}

// RunLoop 一直轮询，直到 ctx 取消。每次只领一个任务，跑完再领下一个，
// Slice 1 不需要并发 Worker，并发会让 SQLite 和步骤序号变复杂。
func (w *Worker) RunLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ran, err := w.RunOne(ctx)
		if err != nil {
			// Worker 本身的错误不能让循环退出，记下来等下一轮重试。
			// 任务级的错误在 RunOne 内部已经标成 failed。
			_ = err
		}
		if !ran {
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.PollInterval):
			}
		}
	}
}

// RunOne 领取并执行一个任务。返回 true 表示领到并跑了一个任务。
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	run, err := w.Store.ClaimNext(ctx)
	if err != nil {
		return false, err
	}
	if run == nil {
		return false, nil
	}
	if err := w.execute(ctx, run); err != nil {
		// execute 内部已经把任务标成 failed，这里只返回错误给循环记日志。
		return true, err
	}
	return true, nil
}

func (w *Worker) execute(ctx context.Context, run *store.Run) error {
	// 1. 先读已提交步骤：重启恢复时这些步骤就是断点，Agent 要复用它们。
	priorSteps, err := w.Store.ListSteps(ctx, run.ID)
	if err != nil {
		_ = w.Store.SetStatus(ctx, run.ID, store.StatusFailed, "", fmt.Sprintf("读取已有步骤失败: %v", err))
		return err
	}
	// 2. 组装 Agent，OnStep 把每一步写库。Seq 在写库时按已有最大序号 +1 算，
	//    这样即使进程在两步之间崩掉，序号也不会断、不会重。
	ag := &agent.Agent{Tools: w.Tools, Model: w.Model}
	ag.OnStep = func(ctx context.Context, step *store.RunStep) error {
		seq, err := w.Store.NextSeq(ctx, run.ID)
		if err != nil {
			return err
		}
		step.Seq = seq
		step.RunID = run.ID
		return w.Store.AppendStep(ctx, step)
	}
	report, err := ag.Run(ctx, run, priorSteps)
	if err != nil {
		// 模型决策失败属于任务失败，记一步 error 再标 failed，
		// 前端能从时间线看到是哪一步出的错。
		seq, _ := w.Store.NextSeq(ctx, run.ID)
		_ = w.Store.AppendStep(ctx, &store.RunStep{RunID: run.ID, Seq: seq, Kind: "error", Summary: err.Error(), Error: err.Error()})
		_ = w.Store.SetStatus(ctx, run.ID, store.StatusFailed, "", err.Error())
		return err
	}
	reportJSON, _ := json.Marshal(report)
	// 3. 报告和 completed 状态一起写：报告落盘了任务才算完成，
	//    避免“状态是 completed 但报告是空”的不一致。
	if err := w.Store.SetStatus(ctx, run.ID, store.StatusCompleted, string(reportJSON), ""); err != nil {
		return err
	}
	// 4. 记一步 report，时间线里能看到最终结论。
	seq, _ := w.Store.NextSeq(ctx, run.ID)
	_ = w.Store.AppendStep(ctx, &store.RunStep{RunID: run.ID, Seq: seq, Kind: "report", Summary: report.Cause, ResultJSON: string(reportJSON)})
	// 5. 只有诊断明确指向 OOM 且目标是 Deployment 时，才生成一份
	//    调内存 limit 的待审批提案。Slice 1 只生成、不执行。
	//    证据不足的任务不生成提案，避免模型瞎猜一个修复。
	if report.Confidence == "high" && containsOOM(report.Cause) && run.Kind == "Deployment" {
		_, _ = repair.CreateProposal(w.Store, &repair.Proposal{
			RunID: run.ID, Namespace: run.Namespace, Deployment: run.Name,
			Container: "demo-api", OldValue: "128Mi", NewValue: "256Mi",
		})
	}
	return nil
}

func containsOOM(s string) bool {
	return strings.Contains(s, "OOM") || strings.Contains(s, "内存")
}

// 确保 model 包被引用，避免以后换模型时漏掉导入（Slice 1 用 stub）。
var _ agent.Model = model.NewStub()
