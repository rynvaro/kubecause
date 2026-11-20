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
