package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// 调试 Pod 的固定配置：
// namespace 用 default；hostPID + privileged 才能 chroot 进宿主机；
// 容器只 sleep，不跑任何用户负载；每次工具调用建一个、用完就删。
const (
	debugNamespace = "default"
	debugPodPrefix = "node-debugger-"
	debugContainer = "debugger"
	debugVolume    = "host-root"
	debugHostPath  = "/"
	debugMountPath = "/host"
	debugSleepArg  = "infinity"
)

// 环境变量：
// MCP_DEBUG_IMAGE 调试 Pod 镜像，默认 busybox:latest；
// MCP_DEBUG_POD_TIMEOUT_SECONDS 等 Pod 进入 Running 的超时秒数，默认 90；
// MCP_DEBUG_EXEC_TIMEOUT_SECONDS 每次 exec 的超时秒数，默认 30。
const (
	defaultDebugImage      = "busybox:latest"
	defaultPodTimeoutSecs  = 90
	defaultExecTimeoutSecs = 30
)

func debugImage() string {
	if v := os.Getenv("MCP_DEBUG_IMAGE"); v != "" {
		return v
	}
	return defaultDebugImage
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// DebugPodExecutor 是 NodeExecutor 的真实实现：
// 每次调用在目标节点上起一个调试 Pod（kubectl debug 的做法，
// 只用 client-go，不调 kubectl 二进制，不走 SSH），
// 等它 Running 后用 SPDY exec 跑一条固定命令，用完删 Pod。
type DebugPodExecutor struct {
	client      kubernetes.Interface
	restCfg     *rest.Config
	image       string
	podTimeout  time.Duration
	execTimeout time.Duration
}

// NewDebugPodExecutor 读 kubeconfig（KUBECONFIG 环境变量或 ~/.kube/config，
// 和主程序一样的规则）建客户端。建客户端本身不连 API Server，
// 集群连不上时在第一次工具调用里报错。
func NewDebugPodExecutor(kubeconfig string) (*DebugPodExecutor, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("加载 kubeconfig 失败（请确认 KUBECONFIG 指向的集群可达，或 ~/.kube/config 存在）: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("创建 Kubernetes 客户端失败: %w", err)
	}
	return &DebugPodExecutor{
		client:      cs,
		restCfg:     cfg,
		image:       debugImage(),
		podTimeout:  time.Duration(envInt("MCP_DEBUG_POD_TIMEOUT_SECONDS", defaultPodTimeoutSecs)) * time.Second,
		execTimeout: time.Duration(envInt("MCP_DEBUG_EXEC_TIMEOUT_SECONDS", defaultExecTimeoutSecs)) * time.Second,
	}, nil
}

// ExecOnNode 在目标节点上跑一条固定命令，返回合并后的 stdout/stderr。
// 每次调用建一个调试 Pod，用完就删（defer 清理，出错路径也删）。
func (e *DebugPodExecutor) ExecOnNode(ctx context.Context, node string, command []string) (string, error) {
	if err := ValidateNodeName(node); err != nil {
		return "", err
	}
	if len(command) == 0 {
		return "", fmt.Errorf("命令不能为空")
	}
	// 节点不存在时先快速失败，不用等建 Pod 超时。
	if _, err := e.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("节点 %q 查不到: %w", node, err)
	}
	podName := debugPodName(node)
	pod := debugPod(podName, node, e.image)
	if _, err := e.client.CoreV1().Pods(debugNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("创建调试 Pod 失败: %w", err)
	}
	// 用完就删：正常路径和出错路径都删，尽力而为。
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		grace := int64(0)
		propagation := metav1.DeletePropagationBackground
		if err := e.client.CoreV1().Pods(debugNamespace).Delete(delCtx, podName, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			PropagationPolicy:  &propagation,
		}); err != nil {
			log.Printf("删除调试 Pod %s 失败（已尽力清理，请手动检查）: %v", podName, err)
		}
	}()
	if err := e.waitRunning(ctx, podName); err != nil {
		return "", err
	}
	return e.execInPod(ctx, podName, command)
}

// ListNodes 列出集群的全部节点（名字 + Ready 状态）。
// 不起调试 Pod，直接调 Nodes API——列节点本身不需要进宿主机。
func (e *DebugPodExecutor) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	list, err := e.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("列出节点失败: %w", err)
	}
	out := make([]NodeInfo, 0, len(list.Items))
	for _, n := range list.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
			}
		}
		out = append(out, NodeInfo{Name: n.Name, Ready: ready})
	}
	return out, nil
}

// waitRunning 等调试 Pod 进入 Running。Pod 进入 Failed/Succeeded
// （容器没能持续运行，例如镜像有问题）时直接报错，不傻等超时。
func (e *DebugPodExecutor) waitRunning(ctx context.Context, podName string) error {
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, e.podTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := e.client.CoreV1().Pods(debugNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("调试 Pod %s 进入 %s 状态（容器没能持续运行，请检查镜像 %q 是否可用）", podName, pod.Status.Phase, e.image)
		default:
			return false, nil
		}
	})
	if err != nil {
		return fmt.Errorf("等待调试 Pod %s 进入 Running 超时（%s）：可能是镜像拉取失败或节点不可调度: %w", podName, e.podTimeout, err)
	}
	return nil
}

// execInPod 用 SPDY executor（exec 子资源）在调试 Pod 里跑一条命令，
// 收 stdout/stderr。命令是 exec 的 argv，不经过 shell。
func (e *DebugPodExecutor) execInPod(ctx context.Context, podName string, command []string) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, e.execTimeout)
	defer cancel()
	req := e.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(debugNamespace).
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("创建 exec 执行器失败: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("在调试 Pod 里执行命令失败: %w", err)
	}
	out := stdout.String()
	if stderr.Len() > 0 {
		out += "\n[stderr]\n" + stderr.String()
	}
	return out, nil
}

// debugPodName 生成调试 Pod 名：node-debugger-<节点名>-<6 位随机小写十六进制>，
// 总长不超过 63 字符（Pod 名的 DNS 子域名上限），节点名过长时截断节点名部分。
func debugPodName(node string) string {
	const maxLen = 63
	suffix := randomHex(6)
	name := debugPodPrefix + node + "-" + suffix
	if len(name) <= maxLen {
		return name
	}
	keep := maxLen - len(debugPodPrefix) - 1 - len(suffix)
	if keep < 1 {
		keep = 1
	}
	return debugPodPrefix + node[:keep] + "-" + suffix
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 几乎不会失败；失败时退到时间戳，避免整个调用失败。
		return fmt.Sprintf("%06x", time.Now().UnixNano()%(1<<24))
	}
	return hex.EncodeToString(b)[:n]
}

// debugPod 生成调试 Pod 的定义：hostPID + privileged，挂载节点根目录到 /host，
// 容器只 sleep，不跑任何用户负载。
func debugPod(name, node, image string) *corev1.Pod {
	privileged := true
	dirType := corev1.HostPathDirectory
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: debugNamespace,
			Labels: map[string]string{
				"app":                     "kubecause-node-debugger",
				"kubecause.io/managed-by": "mcp-linux",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			HostPID:       true,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    debugContainer,
				Image:   image,
				Command: []string{"sleep", debugSleepArg},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      debugVolume,
					MountPath: debugMountPath,
				}},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged,
				},
			}},
			Volumes: []corev1.Volume{{
				Name: debugVolume,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: debugHostPath,
						Type: &dirType,
					},
				},
			}},
		},
	}
}
