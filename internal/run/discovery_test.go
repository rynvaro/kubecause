package run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubecause/internal/kube"
)

// TestDiscoveryToolsFake 覆盖 Slice 4 的 4 个发现工具（fake 数据）：
// demo 命名空间、1 个 Deployment、1 个有问题的 Pod（OOM 重启 5 次）。
func TestDiscoveryToolsFake(t *testing.T) {
	ctx := context.Background()
	tools := &ToolsAdapter{Kube: kube.NewFake()}

	ns, err := tools.ListNamespaces(ctx)
	if err != nil {
		t.Fatalf("list_namespaces 失败: %v", err)
	}
	if !strings.Contains(ns.Summary, "demo") {
		t.Fatalf("命名空间列表应包含 demo，得到 %q", ns.Summary)
	}

	deps, err := tools.ListDeployments(ctx, "")
	if err != nil {
		t.Fatalf("list_deployments 失败: %v", err)
	}
	if countOfData(deps.Data) != 1 {
		t.Fatalf("Deployment 数应为 1，得到 %+v", deps.Data)
	}

	pp, err := tools.ListProblemPods(ctx, "demo")
	if err != nil {
		t.Fatalf("list_problem_pods 失败: %v", err)
	}
	if countOfData(pp.Data) != 1 {
		t.Fatalf("有问题 Pod 数应为 1，得到 %+v", pp.Data)
	}
	if !strings.Contains(pp.Summary, "demo-api") && !strings.Contains(mustJSON(pp.Data), "demo-api") {
		t.Fatalf("有问题 Pod 应是 demo-api，得到 %+v", pp.Data)
	}

	ov, err := tools.GetClusterOverview(ctx)
	if err != nil {
		t.Fatalf("get_cluster_overview 失败: %v", err)
	}
	b, _ := json.Marshal(ov.Data)
	var v struct {
		Namespaces          int `json:"namespaces"`
		Deployments         int `json:"deployments"`
		ProblemPods         int `json:"problemPods"`
		NotReadyDeployments int `json:"notReadyDeployments"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("总览结构解析失败: %v", err)
	}
	if v.Namespaces != 1 || v.Deployments != 1 || v.ProblemPods != 1 {
		t.Fatalf("总览应为 1 命名空间 / 1 Deployment / 1 有问题 Pod，得到 %+v", v)
	}
	// 系统命名空间必须被拒绝。
	if _, err := tools.ListProblemPods(ctx, "kube-system"); err == nil {
		t.Fatalf("kube-system 应被发现工具拒绝")
	}
	if _, err := tools.ListDeployments(ctx, "kube-system"); err == nil {
		t.Fatalf("kube-system 应被发现工具拒绝")
	}
}

func countOfData(data any) int {
	b, _ := json.Marshal(data)
	var v struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return -1
	}
	return v.Count
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
