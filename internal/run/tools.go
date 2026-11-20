// Package run 是 Worker 和工具适配器的所在地。
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubecause/internal/agent"
	"kubecause/internal/knowledge"
	"kubecause/internal/kube"
)

// ToolsAdapter 把 agent.Tools 的 6 个工具分发到 kube 客户端和知识库。
// 它是 Agent 和 K8s 之间的最后一层：这里负责把 K8s 对象转成
// 模型能看懂的摘要和结构化证据，并在这里截断日志。
type ToolsAdapter struct {
	Kube kube.KubeClient
}

// GetPod 返回 Pod 的状态、重启次数和上一次终止原因。
func (t *ToolsAdapter) GetPod(ctx context.Context, namespace, name string) (agent.ToolOutput, error) {
	pod, err := t.Kube.GetPod(ctx, namespace, name)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	restarts := 0
	lastReason := ""
	for _, cs := range pod.Status.ContainerStatuses {
		restarts += int(cs.RestartCount)
		if cs.LastTerminationState.Terminated != nil {
			lastReason = cs.LastTerminationState.Terminated.Reason
		}
	}
	data := map[string]any{
		"pod": pod.Name, "namespace": pod.Namespace, "phase": string(pod.Status.Phase),
		"restartCount": restarts, "lastTerminationReason": lastReason,
		"deployment": kube.PodDeploymentName(pod),
	}
	summary := fmt.Sprintf("Pod %s 状态 %s，重启 %d 次，上次终止原因 %s", pod.Name, pod.Status.Phase, restarts, lastReason)
	if lastReason == "" {
		summary = fmt.Sprintf("Pod %s 状态 %s，重启 %d 次，无终止记录", pod.Name, pod.Status.Phase, restarts)
	}
	return agent.ToolOutput{Summary: summary, Data: data}, nil
}

// ListWorkloadPods 按 Deployment 的 selector 列出所属 Pod，
// 并核对 Pod 确实属于这个 Deployment，防止模型把别的 Pod 混进来。
func (t *ToolsAdapter) ListWorkloadPods(ctx context.Context, namespace, deployment string) (agent.ToolOutput, error) {
	dep, err := t.Kube.GetDeployment(ctx, namespace, deployment)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	labels := map[string]string{}
	if dep.Spec.Selector != nil {
		labels = dep.Spec.Selector.MatchLabels
	}
	pods, err := t.Kube.ListPodsByLabels(ctx, namespace, labels)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	data := map[string]any{"deployment": deployment, "pods": names, "count": len(names)}
	return agent.ToolOutput{
		Summary: fmt.Sprintf("Deployment %s 下有 %d 个 Pod：%s", deployment, len(names), strings.Join(names, ", ")),
		Data:    data,
	}, nil
}

// GetEvents 返回该命名空间下和目标相关的事件，只取最近 20 条，
// 避免把全量 Events 塞进模型上下文。
func (t *ToolsAdapter) GetEvents(ctx context.Context, namespace, name string) (agent.ToolOutput, error) {
	events, err := t.Kube.ListEvents(ctx, namespace)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	var kept []map[string]string
	for _, e := range events {
		if name != "" && e.InvolvedObject.Name != name && !strings.Contains(e.Message, name) {
			// 只保留和目标 Pod/Deployment 相关的事件。
			// 名字为空时不过滤，返回全部（Slice 1 的量很小）。
			if e.InvolvedObject.Name != "" {
				continue
			}
		}
		kept = append(kept, map[string]string{"reason": e.Reason, "message": e.Message, "object": e.InvolvedObject.Name})
		if len(kept) >= 20 {
			break
		}
	}
	reasons := make([]string, 0, len(kept))
	for _, k := range kept {
		reasons = append(reasons, k["reason"])
	}
	data := map[string]any{"events": kept}
	return agent.ToolOutput{
		Summary: fmt.Sprintf("查到 %d 条相关事件：%s", len(kept), strings.Join(reasons, ", ")),
		Data:    data,
	}, nil
}

// GetPodLogs 读指定容器的日志，截断后返回。
func (t *ToolsAdapter) GetPodLogs(ctx context.Context, namespace, pod, container string, previous bool) (agent.ToolOutput, error) {
	raw, err := t.Kube.GetPodLogs(ctx, namespace, pod, container, previous)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	truncated, wasTruncated := agent.TruncateLog(raw)
	data := map[string]any{"pod": pod, "container": container, "previous": previous, "log": truncated, "truncated": wasTruncated}
	summary := fmt.Sprintf("Pod %s 容器 %s 的日志（previous=%v）", pod, container, previous)
	if strings.Contains(raw, "OOMKilled") || strings.Contains(raw, "out of memory") {
		summary += "，日志中出现 OOMKilled / out of memory"
	}
	if wasTruncated {
		summary += "，日志已按 100 行 / 64KB 截断"
	}
	return agent.ToolOutput{Summary: summary, Data: data}, nil
}

// GetDeployment 返回 Deployment 的副本数和容器内存 limit/request。
func (t *ToolsAdapter) GetDeployment(ctx context.Context, namespace, name string) (agent.ToolOutput, error) {
	dep, err := t.Kube.GetDeployment(ctx, namespace, name)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	containers := make([]map[string]string, 0, len(dep.Spec.Template.Spec.Containers))
	for _, c := range dep.Spec.Template.Spec.Containers {
		containers = append(containers, map[string]string{
			"name": c.Name, "image": c.Image,
			"memoryLimit":   c.Resources.Limits.Memory().String(),
			"memoryRequest": c.Resources.Requests.Memory().String(),
		})
	}
	data := map[string]any{"deployment": dep.Name, "replicas": dep.Spec.Replicas, "containers": containers}
	summary := fmt.Sprintf("Deployment %s", dep.Name)
	if len(containers) > 0 {
		summary += fmt.Sprintf("，容器 %s 内存 limit %s", containers[0]["name"], containers[0]["memoryLimit"])
	}
	return agent.ToolOutput{Summary: summary, Data: data}, nil
}

// SearchRunbooks 调知识库，返回带来源的片段。
func (t *ToolsAdapter) SearchRunbooks(ctx context.Context, query string) (agent.ToolOutput, error) {
	snippets := knowledge.Search(query)
	if len(snippets) == 0 {
		return agent.ToolOutput{Summary: fmt.Sprintf("检索 %q 未命中相关手册", query), Data: map[string]any{"query": query, "snippets": []any{}}}, nil
	}
	titles := make([]string, 0, len(snippets))
	for _, s := range snippets {
		titles = append(titles, s.Title)
	}
	b, _ := json.Marshal(snippets)
	_ = b
	return agent.ToolOutput{
		Summary: fmt.Sprintf("检索 %q 命中手册：%s", query, strings.Join(titles, "；")),
		Data:    map[string]any{"query": query, "snippets": snippets},
	}, nil
}
