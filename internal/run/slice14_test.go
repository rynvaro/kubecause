package run

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"kubecause/internal/kube"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podRows 把工具 Data 里的 pods 数组解析成行对象：
// 以前 ListWorkloadPods 的 pods 是名字字符串数组，
// 解析成 []map 会失败，测试就是要抓住这种形状回归。
func podRows(t *testing.T, data any) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(data)
	var v struct {
		Pods  []map[string]any `json:"pods"`
		Count int               `json:"count"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("pods 应是行对象数组，解析失败: %v（数据 %+v）", err, data)
	}
	if v.Count != len(v.Pods) {
		t.Fatalf("count 应等于行数，得到 count=%d 行数=%d", v.Count, len(v.Pods))
	}
	return v.Pods
}

// TestListPodsReturnsAllPodsFullRows 覆盖 v0.14.0 新加的 list_pods：
// demo 下全部 Pod（不限于有问题的），每条是完整行，
// 不是名字字符串。fake 里 demo 只有 1 个 Pod（OOM 重启 5 次）。
func TestListPodsReturnsAllPodsFullRows(t *testing.T) {
	ctx := context.Background()
	tools := &ToolsAdapter{Kube: kube.NewFake()}

	out, err := tools.ListPods(ctx, "demo")
	if err != nil {
		t.Fatalf("list_pods 失败: %v", err)
	}
	rows := podRows(t, out.Data)
	if len(rows) != 1 {
		t.Fatalf("demo 下应有 1 个 Pod，得到 %d 行：%+v", len(rows), rows)
	}
	row := rows[0]
	if row["name"] != kube.DemoPod || row["namespace"] != "demo" {
		t.Fatalf("行里应是 demo-api 的 Pod，得到 %+v", row)
	}
	if row["ready"] != "1/1" {
		t.Fatalf("fake 的 demo-api 容器是 Ready，行里就绪应是 1/1，得到 %+v", row)
	}
	if row["restarts"] != float64(5) {
		t.Fatalf("fake 的 demo-api 应重启 5 次，得到 %+v", row)
	}
	if row["deployment"] != "demo-api" || row["lastTerminationReason"] != "OOMKilled" {
		t.Fatalf("行里应带归属 Deployment 和 OOMKilled 原因，得到 %+v", row)
	}

	// namespace 为空表示全部命名空间：fake 里只有 demo 有 Pod，同样是 1 个。
	all, err := tools.ListPods(ctx, "")
	if err != nil {
		t.Fatalf("list_pods（全部）失败: %v", err)
	}
	if len(podRows(t, all.Data)) != 1 {
		t.Fatalf("全部命名空间应有 1 个 Pod，得到 %+v", all.Data)
	}
}

// TestListWorkloadPodsReturnsFullRows 覆盖 v0.14.0 的形状统一：
// ListWorkloadPods 不再返回名字字符串数组，
// 返回和 ListProblemPods 同形的完整行，前端直接渲染成表格。
func TestListWorkloadPodsReturnsFullRows(t *testing.T) {
	ctx := context.Background()
	tools := &ToolsAdapter{Kube: kube.NewFake()}

	out, err := tools.ListWorkloadPods(ctx, "demo", "demo-api")
	if err != nil {
		t.Fatalf("list_workload_pods 失败: %v", err)
	}
	rows := podRows(t, out.Data)
	if len(rows) != 1 {
		t.Fatalf("demo-api 下应有 1 个 Pod，得到 %+v", rows)
	}
	row := rows[0]
	if row["name"] != kube.DemoPod || row["namespace"] != "demo" || row["deployment"] != "demo-api" {
		t.Fatalf("行应是 demo-api 的完整行，得到 %+v", row)
	}
	if row["ready"] != "1/1" || row["restarts"] != float64(5) {
		t.Fatalf("行里应带就绪和重启次数，得到 %+v", row)
	}
	// 形状回归：pods 再也不是名字字符串数组，
	// 直接按 []string 解析必须失败，证明字符串形状已经不存在。
	b, _ := json.Marshal(out.Data)
	var raw struct {
		Pods []string `json:"pods"`
	}
	if err := json.Unmarshal(b, &raw); err == nil {
		t.Fatalf("pods 不应再是名字字符串数组，得到 %+v", raw.Pods)
	}
}

// fallbackKube 模拟 Deployment 已被删、但孤儿 Pod 还在的集群：
// GetDeployment 报错，ListPodsByLabels 按 app 标签还能找到 Pod。
// 嵌进 kube.KubeClient 接口，只改写这两个方法，别的不调。
type fallbackKube struct {
	kube.KubeClient
}

func (f *fallbackKube) GetDeployment(ctx context.Context, ns, name string) (*appsv1.Deployment, error) {
	return nil, fmt.Errorf("deployments.apps %q not found", name)
}

func (f *fallbackKube) ListPodsByLabels(ctx context.Context, ns string, labels map[string]string) ([]corev1.Pod, error) {
	return []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: ns, Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", Ready: true, RestartCount: 0,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}}, nil
}

// TestListWorkloadPodsFallbackReturnsFullRows 覆盖 Deployment 查不到时的兜底：
// 按 app 标签找到的孤儿 Pod 也要返回完整行，不能退回名字字符串。
func TestListWorkloadPodsFallbackReturnsFullRows(t *testing.T) {
	ctx := context.Background()
	tools := &ToolsAdapter{Kube: &fallbackKube{}}

	out, err := tools.ListWorkloadPods(ctx, "demo", "web")
	if err != nil {
		t.Fatalf("Deployment 查不到时应走标签兜底、不应报错，得到 %v", err)
	}
	rows := podRows(t, out.Data)
	if len(rows) != 1 || rows[0]["name"] != "web-abc123" {
		t.Fatalf("兜底也应返回完整行，得到 %+v", rows)
	}
	if rows[0]["ready"] != "1/1" || rows[0]["phase"] != "Running" {
		t.Fatalf("兜底的行也应带状态，得到 %+v", rows[0])
	}
	b, _ := json.Marshal(out.Data)
	var v struct {
		DeploymentNotFound bool `json:"deploymentNotFound"`
	}
	if err := json.Unmarshal(b, &v); err != nil || !v.DeploymentNotFound {
		t.Fatalf("兜底结果应带 deploymentNotFound 标记，得到 %s", string(b))
	}
}
