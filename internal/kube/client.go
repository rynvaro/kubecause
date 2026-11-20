// Package kube 封装 client-go 的只读访问。
//
// Slice 1 的 fake 和 kind 共用同一个 KubeClient 接口，
// 切换只靠 KUBE_MODE 环境变量，不改业务代码。
// 诊断用的都是只读操作，写操作在 Slice 3 才有，
// 而且要用另一个独立的、权限更小的身份。
package kube

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeClient 是诊断需要的最小只读接口。
// 故意不暴露通用的 clientset，避免业务代码绕过封装
// 去调任意 API，这是只读边界的一道防线。
type KubeClient interface {
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	ListPodsByLabels(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error)
	GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error)
	GetReplicaSet(ctx context.Context, namespace, name string) (*appsv1.ReplicaSet, error)
	ListEvents(ctx context.Context, namespace string) ([]corev1.Event, error)
	// GetPodLogs 只读日志，调用方必须限制行数和字节数，见 agent.TruncateLog。
	GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool) (string, error)
}

// 演示数据，fake 和文档里的验收场景一致：
// demo 命名空间、demo-api Deployment、一个曾 OOMKilled 的 Pod。
const (
	DemoNamespace  = "demo"
	DemoDeployment = "demo-api"
	DemoPod        = "demo-api-7d9f6c4b5-xk2v9"
	DemoContainer  = "demo-api"
	DemoReplicaSet = "demo-api-7d9f6c4b5"
)

// Fake 是 fake clientset 实现，预置演示数据。
// 它还记录每个方法的调用次数，供测试验证“重启恢复不重复执行”。
type Fake struct {
	client    *fake.Clientset
	Logs      map[string]string // pod 名 -> 日志内容
	CallCount map[string]int
	Scenario  string // oom / imagepull / probe，eval 用它区分用例
}

// NewFakeWithScenario 按场景预置不同的 Pod / Events / 日志。
// oom 是 Slice 1 的默认场景；imagepull 和 probe 是 Slice 2 给 eval 加的，
// 这样三个用例不用共用同一份 OOM 数据，评估才有意义。
// 实现上先复用 NewFake 的 OOM 底子，再按场景替换 Pod 状态、
// Events 和日志，避免把整套 Deployment / ReplicaSet 再写一遍。
func NewFakeWithScenario(scenario string) *Fake {
	f := NewFake()
	f.Scenario = scenario
	if scenario == "" || scenario == "oom" {
		return f
	}
	ctx := context.Background()
	// 先清掉 OOM 的 Events，否则 Stub 会优先命中 OOM，掩盖本场景的信号。
	if list, err := f.client.CoreV1().Events(DemoNamespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, e := range list.Items {
			_ = f.client.CoreV1().Events(DemoNamespace).Delete(ctx, e.Name, metav1.DeleteOptions{})
		}
	}
	switch scenario {
	case "imagepull":
		// Deployment 的镜像改成一个不存在的 tag，Pod 卡在拉取阶段。
		if dep, err := f.client.AppsV1().Deployments(DemoNamespace).Get(ctx, DemoDeployment, metav1.GetOptions{}); err == nil {
			dep.Spec.Template.Spec.Containers[0].Image = "demo-api:bad-tag"
			_, _ = f.client.AppsV1().Deployments(DemoNamespace).Update(ctx, dep, metav1.UpdateOptions{})
		}
		if pod, err := f.client.CoreV1().Pods(DemoNamespace).Get(ctx, DemoPod, metav1.GetOptions{}); err == nil {
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: DemoContainer, Ready: false, RestartCount: 0,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image \"demo-api:bad-tag\""}},
			}}
			_, _ = f.client.CoreV1().Pods(DemoNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		}
		for _, e := range []corev1.Event{
			{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-pull.1", Namespace: DemoNamespace}, Reason: "Failed", Message: "Failed to pull image \"demo-api:bad-tag\": rpc error: code = NotFound desc = manifest unknown", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-pull.2", Namespace: DemoNamespace}, Reason: "BackOff", Message: "Back-off pulling image \"demo-api:bad-tag\"", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
		} {
			ev := e
			_, _ = f.client.CoreV1().Events(DemoNamespace).Create(ctx, &ev, metav1.CreateOptions{})
		}
		f.Logs[DemoPod] = "2026-11-20T10:00:01Z demo-api pulling image demo-api:bad-tag\n2026-11-20T10:00:02Z Failed to pull image \"demo-api:bad-tag\": manifest unknown\n2026-11-20T10:00:03Z Pod demo-api-7d9f6c4b5-xk2v9 is in ImagePullBackOff\n"
	case "probe":
		if pod, err := f.client.CoreV1().Pods(DemoNamespace).Get(ctx, DemoPod, metav1.GetOptions{}); err == nil {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: DemoContainer, Ready: false, RestartCount: 3,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, Message: "liveness probe failed"}},
			}}
			_, _ = f.client.CoreV1().Pods(DemoNamespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		}
		for _, e := range []corev1.Event{
			{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-probe.1", Namespace: DemoNamespace}, Reason: "Unhealthy", Message: "Liveness probe failed: HTTP probe failed with statuscode: 500", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-probe.2", Namespace: DemoNamespace}, Reason: "Unhealthy", Message: "Readiness probe failed: HTTP probe failed with statuscode: 500", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
		} {
			ev := e
			_, _ = f.client.CoreV1().Events(DemoNamespace).Create(ctx, &ev, metav1.CreateOptions{})
		}
		f.Logs[DemoPod] = "2026-11-20T10:00:01Z demo-api starting, listening on :8080\n2026-11-20T10:00:12Z liveness probe failed: HTTP probe failed with statuscode: 500\n2026-11-20T10:00:13Z container demo-api restarted, restartCount=3\n"
	}
	return f
}

// NewFake 预置 demo-api 的 Deployment、ReplicaSet、Pod、Events 和日志。
func NewFake() *Fake {
	replicas := int32(1)
	limit := resource.MustParse("128Mi")
	request := resource.MustParse("64Mi")
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: DemoDeployment, Namespace: DemoNamespace, UID: "uid-demo-api-deploy"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo-api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: DemoContainer, Image: "demo-api:latest",
					Resources: corev1.ResourceRequirements{
						Limits:   corev1.ResourceList{corev1.ResourceMemory: limit},
						Requests: corev1.ResourceList{corev1.ResourceMemory: request},
					},
				}}},
			},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: DemoReplicaSet, Namespace: DemoNamespace, UID: "uid-demo-api-rs",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: DemoDeployment, UID: "uid-demo-api-deploy"}},
			Labels:          map[string]string{"app": "demo-api"},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo-api", "pod-template-hash": "7d9f6c4b5"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: DemoContainer, Image: "demo-api:latest"}}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: DemoPod, Namespace: DemoNamespace,
			Labels: map[string]string{"app": "demo-api", "pod-template-hash": "7d9f6c4b5"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: DemoReplicaSet, UID: "uid-demo-api-rs"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: DemoContainer, Image: "demo-api:latest"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: DemoContainer, Ready: true, RestartCount: 5,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled", ExitCode: 137,
						Message: "container exceeded memory limit 128Mi",
					},
				},
			}},
		},
	}
	events := []corev1.Event{
		{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-oom.1", Namespace: DemoNamespace}, Reason: "OOMKilling", Message: "Memory cgroup out of memory: Killed process in container demo-api", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-backoff.1", Namespace: DemoNamespace}, Reason: "BackOff", Message: "Back-off restarting failed container demo-api in pod demo-api-7d9f6c4b5-xk2v9_demo(123)", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
		{ObjectMeta: metav1.ObjectMeta{Name: "demo-api-started.1", Namespace: DemoNamespace}, Reason: "Started", Message: "Started container demo-api", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: DemoPod, Namespace: DemoNamespace}},
	}
	var objs []interface{}
	_ = objs
	cs := fake.NewSimpleClientset(deploy, rs, pod)
	for _, e := range events {
		ev := e
		_, _ = cs.CoreV1().Events(DemoNamespace).Create(context.Background(), &ev, metav1.CreateOptions{})
	}
	logs := map[string]string{
		DemoPod: "2026-11-20T10:00:01Z demo-api starting, memory limit 128Mi\n" +
			"2026-11-20T10:00:12Z demo-api handling request /api/orders\n" +
			"2026-11-20T10:00:31Z fatal error: runtime: out of memory, heap grew to 130Mi\n" +
			"2026-11-20T10:00:31Z container demo-api terminated, reason OOMKilled, exit code 137\n" +
			"2026-11-20T10:01:05Z demo-api starting again after restart, restartCount=5\n",
	}
	return &Fake{client: cs, Logs: logs, CallCount: map[string]int{}, Scenario: "oom"}
}

func (f *Fake) inc(name string) { f.CallCount[name]++ }

func (f *Fake) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	f.inc("GetPod")
	return f.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (f *Fake) ListPodsByLabels(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	f.inc("ListPodsByLabels")
	sel := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: labels})
	list, err := f.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (f *Fake) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	f.inc("GetDeployment")
	return f.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (f *Fake) GetReplicaSet(ctx context.Context, namespace, name string) (*appsv1.ReplicaSet, error) {
	f.inc("GetReplicaSet")
	return f.client.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (f *Fake) ListEvents(ctx context.Context, namespace string) ([]corev1.Event, error) {
	f.inc("ListEvents")
	list, err := f.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (f *Fake) GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool) (string, error) {
	f.inc("GetPodLogs")
	// fake clientset 不支持 Pod 的 log 子资源，这里返回预置的日志字符串。
	// 真集群走 Real.GetPodLogs，用 PodLogOptions 读。
	if log, ok := f.Logs[pod]; ok {
		if previous {
			return log, nil
		}
		return log, nil
	}
	return "", fmt.Errorf("找不到 Pod %s 的日志", pod)
}

// Real 是真 client-go 实现，读 ~/.kube/config 或 KUBECONFIG。
type Real struct {
	client kubernetes.Interface
}

// NewReal 加载 kubeconfig。找不到配置时报错要清楚，
// 让用户知道是没装 kind、没建集群，还是 KUBECONFIG 指错了。
func NewReal(kubeconfig string) (*Real, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("加载 kubeconfig 失败（请确认已执行 kind create cluster，且 ~/.kube/config 存在，或用 KUBECONFIG 指定）: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 Kubernetes 客户端失败: %w", err)
	}
	return &Real{client: cs}, nil
}

func (r *Real) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return r.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *Real) ListPodsByLabels(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	sel := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: labels})
	list, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *Real) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return r.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *Real) GetReplicaSet(ctx context.Context, namespace, name string) (*appsv1.ReplicaSet, error) {
	return r.client.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *Real) ListEvents(ctx context.Context, namespace string) ([]corev1.Event, error) {
	list, err := r.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *Real) GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool) (string, error) {
	// 只读最近的日志，并限制行数和字节数，避免把全量日志拉回来。
	tail := int64(100)
	limit := int64(64 * 1024)
	req := r.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: previous, TailLines: &tail, LimitBytes: &limit,
	})
	b, err := req.Do(ctx).Raw()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---------------------------------------------------------------------------
// 写客户端：和只读的 KubeClient 完全分开的接口。
//
// 为什么要分开：诊断只读，修复要写，两个操作的安全边界不一样。
// 生产上读身份和写身份应该是两个不同的 ServiceAccount，写身份只允许
// 在演示 namespace 改指定 Deployment 的内存 limit（见 scripts/kind/rbac.yaml）。
// 本地 kind 演示为了简单仍用同一个 kubeconfig，但代码路径已经分开：
// 读走 NewReal，写走 NewRealRepair；以后只要把 NewRealRepair 换成
// 另一个 kubeconfig（REPAIR_KUBECONFIG）就行，修复模块不用改。
// RepairClient 只暴露修复需要的三个操作，不暴露通用的 clientset，
// 防止修复代码绕过白名单去调任意写 API。
// ---------------------------------------------------------------------------

// RepairClient 是修复专用的写接口。
type RepairClient interface {
	// GetDeploymentForRepair 是执行前校验用的读操作，只在修复流程里用。
	GetDeploymentForRepair(ctx context.Context, namespace, name string) (*appsv1.Deployment, error)
	// PatchDeploymentMemoryLimit 只改指定容器的内存 limit 这一个字段。
	// resourceVersion 是执行前那次读取的版本，写进 Patch 里做并发校验；
	// dryRun 为真时只校验不落盘。其他字段一律不改，调用方（repair 包）
	// 还要在 Patch 前做一次白名单校验，双重防线。
	PatchDeploymentMemoryLimit(ctx context.Context, namespace, name, container, newValue, resourceVersion string, dryRun bool) (*appsv1.Deployment, error)
	// ListPodsForRepair 按标签列 Pod，供 rollout 验证区分新旧 Pod。
	ListPodsForRepair(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error)
}

// NewRealRepair 创建写客户端。kubeconfig 为空时用默认的 ~/.kube/config。
// 生产部署时应传入写身份专用的 kubeconfig（REPAIR_KUBECONFIG），
// 本地演示传入空字符串即可，仍会和读客户端用同一个文件，但对象是独立的。
func NewRealRepair(kubeconfig string) (*Real, error) {
	return NewReal(kubeconfig)
}

// strategicMergePatch 只改一个容器的内存 limit，并把 resourceVersion
// 写进 metadata 做并发校验：API Server 发现版本已变会返回 409，
// 调用方就知道有人在中间改过目标，不能再按旧前提 Patch。
func memoryLimitPatch(container, newValue, resourceVersion string) []byte {
	rv := ""
	if resourceVersion != "" {
		rv = fmt.Sprintf(`"metadata":{"resourceVersion":%q},`, resourceVersion)
	}
	return []byte(fmt.Sprintf(`{%s"spec":{"template":{"spec":{"containers":[{"name":%q,"resources":{"limits":{"memory":%q}}}]}}}}`,
		rv, container, newValue))
}

func (r *Real) GetDeploymentForRepair(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return r.GetDeployment(ctx, namespace, name)
}

func (r *Real) ListPodsForRepair(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	return r.ListPodsByLabels(ctx, namespace, labels)
}

func (r *Real) PatchDeploymentMemoryLimit(ctx context.Context, namespace, name, container, newValue, resourceVersion string, dryRun bool) (*appsv1.Deployment, error) {
	opts := metav1.PatchOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return r.client.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType,
		memoryLimitPatch(container, newValue, resourceVersion), opts)
}

func (f *Fake) GetDeploymentForRepair(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return f.GetDeployment(ctx, namespace, name)
}

func (f *Fake) ListPodsForRepair(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	return f.ListPodsByLabels(ctx, namespace, labels)
}

// PatchDeploymentMemoryLimit 是 Fake 的写实现。
// dryRun 时只在内存里算出结果，不落盘、不模拟 rollout。
// 真实 Patch 时先走和 Real 一样的 clientset Patch，再模拟控制器行为：
// fake clientset 没有 Deployment 控制器，不会自动建新 ReplicaSet / 新 Pod、
// 也不会推进 observedGeneration，不模拟的话 rollout 验证在 fake 上永远过不了。
// 所以这里显式做这三件事：generation +1、status 置为已更新且可用、
// 建一个带新 pod-template-hash 的 ReplicaSet 和一个 Ready 的新 Pod。
// 这只是 fake 的模拟，Real 不走这条路，生产集群由真正的控制器完成 rollout。
func (f *Fake) PatchDeploymentMemoryLimit(ctx context.Context, namespace, name, container, newValue, resourceVersion string, dryRun bool) (*appsv1.Deployment, error) {
	dep, err := f.GetDeployment(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	found := false
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == container {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("Deployment %s/%s 里没有容器 %q", namespace, name, container)
	}
	if _, err := resource.ParseQuantity(newValue); err != nil {
		return nil, fmt.Errorf("内存值非法 %q: %w", newValue, err)
	}
	if dryRun {
		// 只在内存副本上改，不写回 clientset。
		cp := dep.DeepCopy()
		for i := range cp.Spec.Template.Spec.Containers {
			if cp.Spec.Template.Spec.Containers[i].Name == container {
				cp.Spec.Template.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = resource.MustParse(newValue)
			}
		}
		return cp, nil
	}
	patched, err := f.client.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType,
		memoryLimitPatch(container, newValue, resourceVersion), metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	f.simulateRollout(ctx, patched, newValue)
	// 模拟完再读一次，返回带新 status 的 Deployment。
	return f.GetDeployment(ctx, namespace, name)
}

// simulateRollout 模拟控制器在 Patch 后的行为，只在 Fake 里用。
func (f *Fake) simulateRollout(ctx context.Context, dep *appsv1.Deployment, newValue string) {
	const newHash = "9a1b2c3d4"
	// 1. generation +1，status 置为已更新且可用。
	dep.Generation++
	if _, err := f.client.AppsV1().Deployments(dep.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return
	}
	dep.Status.ObservedGeneration = dep.Generation
	dep.Status.Replicas = 1
	dep.Status.UpdatedReplicas = 1
	dep.Status.AvailableReplicas = 1
	dep.Status.ReadyReplicas = 1
	_, _ = f.client.AppsV1().Deployments(dep.Namespace).UpdateStatus(ctx, dep, metav1.UpdateOptions{})

	// 2. 建带新 hash 的 ReplicaSet（已存在时跳过，保证重复 Patch 不报错）。
	rsName := dep.Name + "-" + newHash
	if _, err := f.client.AppsV1().ReplicaSets(dep.Namespace).Get(ctx, rsName, metav1.GetOptions{}); err != nil {
		replicas := int32(1)
		tmpl := *dep.Spec.Template.DeepCopy()
		if tmpl.Labels == nil {
			tmpl.Labels = map[string]string{}
		}
		tmpl.Labels["pod-template-hash"] = newHash
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: rsName, Namespace: dep.Namespace, UID: types.UID("uid-" + rsName),
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID}},
				Labels:          map[string]string{"app": dep.Name, "pod-template-hash": newHash},
			},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": dep.Name, "pod-template-hash": newHash}},
				Template: tmpl,
			},
		}
		_, _ = f.client.AppsV1().ReplicaSets(dep.Namespace).Create(ctx, rs, metav1.CreateOptions{})
	}

	// 3. 建一个 Ready、重启 0 次的新 Pod（已存在时跳过）。
	podName := dep.Name + "-" + newHash + "-abcde"
	if _, err := f.client.CoreV1().Pods(dep.Namespace).Get(ctx, podName, metav1.GetOptions{}); err != nil {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: podName, Namespace: dep.Namespace,
				Labels: map[string]string{"app": dep.Name, "pod-template-hash": newHash},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rsName, UID: types.UID("uid-" + rsName)}},
				CreationTimestamp: metav1.Now(),
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: DemoContainer, Image: "demo-api:latest"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: DemoContainer, Ready: true, RestartCount: 0,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		}
		_, _ = f.client.CoreV1().Pods(dep.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	}
}

// PodDeploymentName 按 Pod -> ReplicaSet -> Deployment 追溯归属。
// 演示和面试时能讲清楚“这个 Pod 属于哪个 Deployment”。
func PodDeploymentName(pod *corev1.Pod) string {
	for _, o := range pod.OwnerReferences {
		if o.Kind == "ReplicaSet" {
			// ReplicaSet 名形如 demo-api-7d9f6c4b5，去掉最后一段 hash 就是 Deployment 名。
			parts := strings.Split(o.Name, "-")
			if len(parts) > 1 {
				return strings.Join(parts[:len(parts)-1], "-")
			}
			return o.Name
		}
		if o.Kind == "Deployment" {
			return o.Name
		}
	}
	return ""
}
