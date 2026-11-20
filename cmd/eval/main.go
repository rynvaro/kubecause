// Command eval 是离线评估的入口（Slice 2）。
//
// 用固定故障样本跑 Agent + 工具接口，复用线上代码，
// 统计诊断命中率、证据引用准确性、工具步数和耗时。
//
// 为什么评估要分 stub 和 real 两档：
//   stub 是确定性剧本，用来验证管线本身（工具、校验、恢复、提案）
//   有没有坏，跑起来快且不花钱；
//   real 是真模型，用来验证模型在同一批样本上的诊断能力，
//   结果会波动，要看命中率和引用准确率，不只看通过与否。
// MODEL_MODE=real 但未设置 MODEL_API_KEY 时跳过真模型评估，
// 明确提示后正常退出，不报错，因为本机可能只是想跑 stub 回归。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/knowledge"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// Case 是 testdata/cases 下的评估用例。
// Scenario 决定 fake 客户端预置哪类故障数据：oom / imagepull / probe，
// 为空时默认 oom，保持和 Slice 1 的 oom.json 兼容。
type Case struct {
	Name          string `json:"caseName"`
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind"`
	Target        string `json:"name"`
	Question      string `json:"question"`
	ExpectContain string `json:"expectContain"` // 诊断原因里应包含的关键词
	Scenario      string `json:"scenario"`
}

type caseResult struct {
	name      string
	pass      bool
	hit       bool
	citation  float64 // 引用准确率：报告里的证据 ID 有多少属于本次任务
	steps     int
	duration  time.Duration
	cause     string
}

func main() {
	modelMode := strings.ToLower(os.Getenv("MODEL_MODE"))
	if modelMode == "" {
		modelMode = "stub"
	}
	// 真模型评估需要 Key，没有就跳过，不报错退出。
	if modelMode == "real" && strings.TrimSpace(os.Getenv("MODEL_API_KEY")) == "" {
		fmt.Println("跳过真模型评估：MODEL_MODE=real 但未设置 MODEL_API_KEY（Key 只从环境变量读，不写进文件）")
		fmt.Println("如需跑真模型评估，请设置 MODEL_API_KEY 后再跑；只跑 stub 回归请用 MODEL_MODE=stub go run ./cmd/eval")
		return
	}

	dir := "testdata/cases"
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("读取用例目录失败: %v\n", err)
		os.Exit(1)
	}
	var results []caseResult
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Printf("读取 %s 失败: %v\n", e.Name(), err)
			continue
		}
		var c Case
		if err := json.Unmarshal(b, &c); err != nil {
			fmt.Printf("解析 %s 失败: %v\n", e.Name(), err)
			continue
		}
		res := runCase(c, modelMode)
		results = append(results, res)
		fmt.Printf("case=%s pass=%v hit=%v citation=%.2f steps=%d duration=%s cause=%q\n",
			res.name, res.pass, res.hit, res.citation, res.steps, res.duration, res.cause)
	}
	if len(results) == 0 {
		fmt.Println("没有找到评估用例")
		os.Exit(1)
	}
	passed := 0
	totalSteps := 0
	var totalDur time.Duration
	totalCitation := 0.0
	for _, r := range results {
		if r.pass {
			passed++
		}
		totalSteps += r.steps
		totalDur += r.duration
		totalCitation += r.citation
	}
	n := float64(len(results))
	fmt.Printf("eval: %d/%d 通过，命中率 %.2f，平均步数 %.1f，平均耗时 %s，平均引用准确率 %.2f（MODEL_MODE=%s）\n",
		passed, len(results), float64(passed)/n, float64(totalSteps)/n, totalDur/time.Duration(len(results)), totalCitation/n, modelMode)
	if passed != len(results) {
		os.Exit(1)
	}
}

func runCase(c Case, modelMode string) caseResult {
	ctx := context.Background()
	scenario := c.Scenario
	if scenario == "" {
		scenario = "oom"
	}
	// 每个用例用内存 SQLite，互不干扰；FTS5 索引也在这个库里建，
	// 这样 eval 走的检索路径和线上一致。
	st, err := store.Open(":memory:")
	if err != nil {
		fmt.Printf("打开内存库失败: %v\n", err)
		return caseResult{name: c.Name}
	}
	defer st.Close()
	kbIdx := knowledge.NewIndex(st.DB())
	for _, dir := range []string{"runbooks", "../runbooks", "../../runbooks"} {
		if err := kbIdx.Build(ctx, dir); err == nil {
			break
		}
	}
	fake := kube.NewFakeWithScenario(scenario)
	tools := &run.ToolsAdapter{Kube: fake, KB: kbIdx}

	var m agent.Model
	if modelMode == "real" {
		real, err := model.NewRealFromEnv()
		if err != nil {
			fmt.Printf("创建真模型失败: %v\n", err)
			return caseResult{name: c.Name}
		}
		m = real
	} else {
		m = model.NewStub()
	}

	ag := &agent.Agent{Tools: tools, Model: m}
	start := time.Now()
	r := &store.Run{Namespace: c.Namespace, Kind: c.Kind, Name: c.Target, Question: c.Question, MaxSteps: 8, TimeoutSeconds: 60}
	if err := st.CreateRun(ctx, r); err != nil {
		return caseResult{name: c.Name}
	}
	ag.OnStep = func(ctx context.Context, step *store.RunStep) error {
		seq, err := st.NextSeq(ctx, r.ID)
		if err != nil {
			return err
		}
		step.Seq = seq
		step.RunID = r.ID
		return st.AppendStep(ctx, step)
	}
	report, err := ag.Run(ctx, r, nil)
	dur := time.Since(start)
	if err != nil || report == nil {
		return caseResult{name: c.Name, duration: dur, cause: fmt.Sprintf("agent error: %v", err)}
	}
	steps, _ := st.ListSteps(ctx, r.ID)
	// 引用准确率：报告里的证据 ID 有多少能在本次任务的步骤里找到。
	valid := map[string]bool{}
	for _, s := range steps {
		if s.CallID != "" {
			valid[s.CallID] = true
		}
		valid[fmt.Sprintf("ev-%d", s.Seq)] = true
	}
	citation := 0.0
	if len(report.EvidenceIDs) > 0 {
		hitIDs := 0
		for _, id := range report.EvidenceIDs {
			if valid[id] {
				hitIDs++
			}
		}
		citation = float64(hitIDs) / float64(len(report.EvidenceIDs))
	}
	// 命中只看 Cause，不看 Facts：Facts 里是工具摘要，探针案的
	// Facts 里也会出现“镜像”二字（检索带回了镜像手册），
	// 只看 Facts 会把错因判成通过，掩盖真正的误判。
	// Slice 2 之前就是这里太宽松，probe 用例 cause 写错也通过了。
	hit := false
	if c.ExpectContain != "" && strings.Contains(report.Cause, c.ExpectContain) {
		hit = true
	}
	return caseResult{
		name: c.Name, pass: hit && len(report.EvidenceIDs) > 0 && citation > 0,
		hit: hit, citation: citation, steps: len(steps), duration: dur, cause: report.Cause,
	}
}
