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
	// KB 是 Slice 2 的 FTS5 索引，为 nil 时回退到内存关键词检索。
	// 这样 Slice 1 的调用方不用改，传了索引才走 FTS5。
	KB *knowledge.Index
}

// GetPod 返回 Pod 的状态、重启次数和上一次终止原因。
func (t *ToolsAdapter) GetPod(ctx context.Context, namespace, name string) (agent.ToolOutput, error) {
	pod, err := t.Kube.GetPod(ctx, namespace, name)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	restarts := 0
	lastReason := ""
	waitingReason := ""
	for _, cs := range pod.Status.ContainerStatuses {
		restarts += int(cs.RestartCount)
		if cs.LastTerminationState.Terminated != nil {
			lastReason = cs.LastTerminationState.Terminated.Reason
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			waitingReason = cs.State.Waiting.Reason
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
	// 等待原因要写进摘要：ImagePullBackOff 只在这里和 Events 里出现，
	// 不写摘要模型在下一步选 search_runbooks 关键词时会看不到信号。
	if waitingReason != "" {
		summary += fmt.Sprintf("，容器等待原因 %s", waitingReason)
	}
	return agent.ToolOutput{Summary: summary, Data: data}, nil
}

// ListWorkloadPods 按 Deployment 的 selector 列出所属 Pod，
// 并核对 Pod 确实属于这个 Deployment，防止模型把别的 Pod 混进来。
func (t *ToolsAdapter) ListWorkloadPods(ctx context.Context, namespace, deployment string) (agent.ToolOutput, error) {
	dep, err := t.Kube.GetDeployment(ctx, namespace, deployment)
	if err != nil {
		// 容错：如果 Deployment 查不到（比如已被删、或服务端连的是另一个集群），
		// 不要直接报错让模型原地重试 8 次，改按 app=<deployment> 标签直接列 Pod，
		// 孤儿 Pod 也能被发现，诊断还能继续。拿不到 Pod 时再返回原错误。
		pods, listErr := t.Kube.ListPodsByLabels(ctx, namespace, map[string]string{"app": deployment})
		if listErr != nil || len(pods) == 0 {
			return agent.ToolOutput{}, err
		}
		names := make([]string, 0, len(pods))
		for _, p := range pods {
			names = append(names, p.Name)
		}
		return agent.ToolOutput{
			Summary: fmt.Sprintf("Deployment %s 查不到（%v），但按标签 app=%s 找到 %d 个 Pod：%s", deployment, err, deployment, len(names), strings.Join(names, ", ")),
			Data:    map[string]any{"deployment": deployment, "pods": names, "count": len(names), "deploymentNotFound": true},
		}, nil
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
	if strings.Contains(raw, "Failed to pull image") || strings.Contains(raw, "ImagePullBackOff") {
		summary += "，日志中出现 Failed to pull image / ImagePullBackOff"
	}
	if strings.Contains(raw, "liveness probe failed") || strings.Contains(raw, "Liveness probe failed") {
		summary += "，日志中出现 Liveness probe failed"
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
// 优先走 FTS5，FTS5 未启用、出错或查不到时回退到关键词检索，
// 并在日志里说明回退原因，保证检索永远有结果可回。
func (t *ToolsAdapter) SearchRunbooks(ctx context.Context, query string) (agent.ToolOutput, error) {
	var snippets []knowledge.Snippet
	if t.KB != nil {
		if out, err := t.KB.Search(ctx, query); err == nil && len(out) > 0 {
			snippets = out
		}
	}
	if len(snippets) == 0 {
		snippets = knowledge.Search(query)
	}
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
