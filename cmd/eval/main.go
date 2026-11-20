// Command eval 是离线评估的最小入口（Slice 1）。
//
// 用固定故障样本跑桩模型 + fake 客户端，复用线上 Agent 与工具接口，
// 统计诊断命中率、证据引用、工具步数和耗时。
// 真模型评估在 Slice 2 接入同一批样本。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// Case 是 testdata/cases 下的评估用例。
type Case struct {
	Name          string `json:"caseName"`
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind"`
	Target        string `json:"name"`
	Question      string `json:"question"`
	ExpectContain string `json:"expectContain"` // 诊断原因里应包含的关键词
}

func main() {
	dir := "testdata/cases"
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("读取用例目录失败: %v\n", err)
		os.Exit(1)
	}
	total, passed := 0, 0
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
		total++
		ok, steps, dur := runCase(c)
		if ok {
			passed++
		}
		fmt.Printf("case=%s pass=%v steps=%d duration=%s\n", c.Name, ok, steps, dur)
	}
	fmt.Printf("eval: %d/%d 通过\n", passed, total)
	if total == 0 || passed != total {
		os.Exit(1)
	}
}

func runCase(c Case) (bool, int, time.Duration) {
	ctx := context.Background()
	// 每个用例用内存 SQLite（:memory:），互不干扰。
	st, err := store.Open(":memory:")
	if err != nil {
		fmt.Printf("打开内存库失败: %v\n", err)
		return false, 0, 0
	}
	defer st.Close()
	fake := kube.NewFake()
	tools := &run.ToolsAdapter{Kube: fake}
	ag := &agent.Agent{Tools: tools, Model: model.NewStub()}
	start := time.Now()
	r := &store.Run{Namespace: c.Namespace, Kind: c.Kind, Name: c.Target, Question: c.Question, MaxSteps: 8, TimeoutSeconds: 60}
	if err := st.CreateRun(ctx, r); err != nil {
		return false, 0, 0
	}
	// 桩 Agent 直接跑，不经过 Worker，方便评估只测诊断本身。
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
		return false, 0, dur
	}
	steps, _ := st.ListSteps(ctx, r.ID)
	// 命中标准：原因包含期望关键词，且至少引用了一条证据。
	hit := false
	if c.ExpectContain != "" {
		for _, f := range append([]string{report.Cause}, report.Facts...) {
			if len(f) >= len(c.ExpectContain) && contains(f, c.ExpectContain) {
				hit = true
				break
			}
		}
	}
	return hit && len(report.EvidenceIDs) > 0, len(steps), dur
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
