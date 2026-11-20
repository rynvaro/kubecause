package run

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"kubecause/internal/agent"
	"kubecause/internal/model"
	"kubecause/internal/repair"
	"kubecause/internal/store"
)

// Worker 是后台任务执行器：轮询领取 queued 任务，跑 Agent 循环，
// 写报告，必要时生成一份待审批的修复提案；诊断没有任务时，
// 再轮询已批准的提案，执行 Patch 并验证 rollout。
// 单个 Go 进程同时跑 HTTP API 和 Worker，关闭浏览器页面后任务继续执行，
// 因为任务状态在 SQLite 里，不在内存里。
type Worker struct {
	Store *store.Store
	Tools agent.Tools
	Model agent.Model
	// Repair 是修复执行器，Slice 3 才有；为 nil 时只做诊断，不执行修复。
	// 这样 Slice 1/2 的调用方（NewWorker）不用改，测试也不用每次都配写客户端。
	Repair *repair.Executor
	// PollInterval 是轮询间隔，Slice 1 用 500ms，演示时几乎立刻开始。
	PollInterval time.Duration
}

// NewWorker 组装 Worker。Model 由调用方按 MODEL_MODE 传入。
func NewWorker(s *store.Store, tools agent.Tools, m agent.Model) *Worker {
	return &Worker{Store: s, Tools: tools, Model: m, PollInterval: 500 * time.Millisecond}
}

// SetRepair 给 Worker 配上修复执行器，main.go 在组装写客户端后调用。
func (w *Worker) SetRepair(e *repair.Executor) {
	w.Repair = e
}

// StartupRecovery 在进程启动时调用：把上次崩掉时停在 running 的任务
// 改回 queued，步骤保留，Agent 会根据已提交步骤续跑。
// 这是“重启恢复不重复执行”的第一步，第二步在 Agent.Run 里做指纹去重。
// 同时把执行中途崩掉的提案改回待执行，重启后重新领取，
// 重跑是安全的（执行前校验会先看当前值是否已是新值）。
func (w *Worker) StartupRecovery(ctx context.Context) (int64, error) {
	n, err := w.Store.ResetRunningToQueued(ctx)
	if err != nil {
		return n, err
	}
	_, _ = w.Store.ResetExecutingChanges(ctx)
	return n, nil
}

// RunLoop 一直轮询，直到 ctx 取消。每次先领一个诊断任务，
// 没有诊断任务时再领一个已批准的修复提案，跑完再领下一个。
// Slice 3 也不需要并发 Worker，并发会让 SQLite 和步骤序号变复杂，
// 而且诊断和修复串行跑，演示时顺序是可预期的。
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
		if ran {
			continue
		}
		// 诊断没有任务时，轮到修复：已批准的提案在这里执行并验证。
		if w.Repair != nil {
			if ranRepair, _ := w.ProcessOneRepair(ctx); ranRepair {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.PollInterval):
		}
	}
}

// ProcessOneRepair 领取并执行一份已批准的提案。
// 返回 true 表示领到并跑了一份提案。拒绝的、过期的提案不会被领到，
// 所以拒绝后无写入在这里就保证了。
func (w *Worker) ProcessOneRepair(ctx context.Context) (bool, error) {
	if w.Repair == nil {
		return false, nil
	}
	return w.Repair.ExecuteApproved(ctx)
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
	// 5. 模型在报告里给了修复建议时，生成一份待审批提案。
	//    通用逻辑（Slice 4）：不再只认 OOM / demo-api / 128Mi->256Mi，
	//    建议的目标、字段、新旧值都来自模型报告里的 SuggestedRepair，
	//    Worker 只做三件事：补齐缺失项（容器名、旧值、新值）、
	//    拿集群现状核对、交给 repair.ValidatePatch 做白名单校验。
	//    校验不通过就不生成提案，不会把一个非法建议变成可审批的东西。
	//    置信度 high 或 medium 都生成：真模型在 kind 上跑 OOM 常常
	//    只给 medium（日志为空、没有实时 metrics，它不敢说 high），
	//    但还要经过人工审批这一关，medium 足够出提案；
	//    low（证据不足）仍不生成，避免模型瞎猜一个修复。
	//    提案生成时把目标 Deployment 的 UID 一起绑上，执行前校验用。
	//    执行本身不在这里，批准后由 Worker 循环里的 ProcessOneRepair 做。
	if report.Confidence == "high" || report.Confidence == "medium" {
		_ = w.maybeCreateProposal(ctx, run, report)
	}
	return nil
}

// maybeCreateProposal 把模型报告里的修复建议转成一份待审批提案。
// 通用逻辑（Slice 9）：不再只认内存，建议的目标和 Patch 列表
// 都来自模型报告里的 SuggestedRepair，Worker 只做三件事：
// 补齐缺失项（容器名、每条 Patch 的改前值）、拿集群现状核对、
// 交给 repair.ValidatePatch / ValidatePatchWithCurrent 做白名单校验。
// 校验不通过就不生成提案，不会把一个非法建议变成可审批的东西。
//   - 目标 Deployment：建议里的 deployment 为空时用本次任务的目标
//     （Kind 为 Deployment 时才有意义，Pod 任务不生成修复提案）；
//   - 容器名：建议里没给时读 Deployment 的第一个容器；
//   - Patch 下标：按容器名重算，保证改的是指明的容器；
//   - 改前值：以集群现状为准，逐条读出来存进提案做展示和执行前提核对；
//   - 新值：必须由模型给，模型没给新值就不生成提案。
//     以前这里新值缺失时按旧值翻倍，那是规则合成在替模型做决策，
//     Slice 6 去掉了：即便有 OOM 这个案例，也不要为了演示写死，
//     修复决策只能由模型做，后端只做校验。
//   - 故障类型不参与判断：OOM 的内存 Patch、ImagePullBackOff 的镜像 Patch、
//     用户主动说的换镜像，都走同一条路，不用每个故障写一份生成逻辑。
//
// 最后统一过 ValidatePatch（CreateProposal 内部会再校验一次），
// 任何一关不过都不生成提案，也不报错让任务失败——
// 没有提案的诊断本身仍然是完整的，报告已经落盘了。
func (w *Worker) maybeCreateProposal(ctx context.Context, run *store.Run, report *agent.Report) error {
	sr := report.SuggestedRepair
	if sr == nil {
		return nil
	}
	patches := sr.ToPatches()
	if len(patches) == 0 {
		return nil
	}
	if run.Kind != "Deployment" {
		return nil
	}
	namespace := sr.Namespace
	if namespace == "" {
		namespace = run.Namespace
	}
	deployment := sr.Deployment
	if deployment == "" {
		deployment = run.Name
	}
	// 读集群现状：拿 UID、容器列表和每条 Patch 的当前值。
	// ToolsAdapter 的 Kube 字段是导出的，这里直接读目标。
	ta, ok := w.Tools.(*ToolsAdapter)
	if !ok || ta.Kube == nil {
		return nil
	}
	dep, err := ta.Kube.GetDeployment(ctx, namespace, deployment)
	if err != nil {
		return nil
	}
	uid := string(dep.UID)
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil
	}
	container := sr.Container
	if container == "" {
		// 容器名不要写死：建议里没给就用 Deployment 的第一个容器，
		// 这是单容器 Deployment 的常规情况；多容器时模型必须指明容器。
		container = dep.Spec.Template.Spec.Containers[0].Name
	}
	// 按容器名重算下标，再按现状做第二层校验（下标越界、内存倍数）。
	patches = repair.ResolveContainerIndex(patches, dep, container)
	if err := repair.ValidatePatch(namespace, deployment, patches); err != nil {
		return nil
	}
	if err := repair.ValidatePatchWithCurrent(dep, patches); err != nil {
		return nil
	}
	// 改前值以集群现状为准：逐条读出来，第一个 Patch 的改前/改后值
	// 存进提案的 OldValue / NewValue 做展示，执行前提核对也用它。
	oldValue, newValue := "", ""
	for i, p := range patches {
		cur, ok := patchCurrentValue(dep, p)
		if !ok {
			return nil
		}
		if i == 0 {
			oldValue, newValue = cur, agent.PatchValueString(p.Value)
		}
		// 内存 limit 的改前值和建议卡里的旧值对不上时放弃：
		// 建议是按过期快照提的，不能用。镜像、副本数没有旧值可对，
		// 以现状为准即可。
		if sr.OldValue != "" && i == 0 && sr.Field == "memoryLimit" && cur != sr.OldValue {
			return nil
		}
	}
	_, _ = repair.CreateProposal(w.Store, &repair.Proposal{
		RunID: run.ID, Namespace: namespace, Deployment: deployment,
		Container: container, OldValue: oldValue, NewValue: newValue,
		Patches: patches, DeploymentUID: uid,
	})
	return nil
}

// patchCurrentValue 从 Deployment 现状里读一条 Patch 对应的当前值，
// 和 repair 层读现状的规则一致（内存/CPU 用规范数量写法，
// 镜像直接比字符串，副本数比数字），worker 包内直接读，
// 避免把 repair 的内部函数导出。
func patchCurrentValue(dep *appsv1.Deployment, p agent.PatchOperation) (string, bool) {
	if dep == nil {
		return "", false
	}
	if p.Path == "/spec/replicas" {
		if dep.Spec.Replicas == nil {
			return "1", true
		}
		return fmt.Sprintf("%d", *dep.Spec.Replicas), true
	}
	m := containerPathRe.FindStringSubmatch(p.Path)
	if m == nil {
		return "", false
	}
	idx := 0
	for _, c := range m[1] {
		idx = idx*10 + int(c-'0')
	}
	if idx >= len(dep.Spec.Template.Spec.Containers) {
		return "", false
	}
	ct := dep.Spec.Template.Spec.Containers[idx]
	switch m[2] {
	case "image":
		return ct.Image, true
	case "resources/limits/memory":
		return ct.Resources.Limits.Memory().String(), true
	case "resources/requests/memory":
		return ct.Resources.Requests.Memory().String(), true
	case "resources/limits/cpu":
		return ct.Resources.Limits.Cpu().String(), true
	case "resources/requests/cpu":
		return ct.Resources.Requests.Cpu().String(), true
	}
	return "", false
}

var containerPathRe = regexp.MustCompile(`^/spec/template/spec/containers/(\d+)/(image|resources/(limits|requests)/(memory|cpu))$`)

// 确保 model 包被引用，避免以后换模型时漏掉导入（Slice 1 用 stub）。
var _ agent.Model = model.NewStub()
