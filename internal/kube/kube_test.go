package kube

import (
	"context"
	"strings"
	"testing"
)

func TestNewFakeWithScenarioImagePull(t *testing.T) {
	f := NewFakeWithScenario("imagepull")
	if f.Scenario != "imagepull" {
		t.Fatalf("Scenario 应为 imagepull，得到 %q", f.Scenario)
	}
	pod, err := f.GetPod(context.Background(), DemoNamespace, DemoPod)
	if err != nil {
		t.Fatalf("取 Pod 失败: %v", err)
	}
	if string(pod.Status.Phase) != "Pending" {
		t.Fatalf("镜像拉取场景 Pod 应为 Pending，得到 %s", pod.Status.Phase)
	}
	if len(pod.Status.ContainerStatuses) == 0 || pod.Status.ContainerStatuses[0].State.Waiting == nil || pod.Status.ContainerStatuses[0].State.Waiting.Reason != "ImagePullBackOff" {
		t.Fatalf("镜像拉取场景容器应处于 ImagePullBackOff，得到 %+v", pod.Status.ContainerStatuses)
	}
	events, err := f.ListEvents(context.Background(), DemoNamespace)
	if err != nil {
		t.Fatalf("取 Events 失败: %v", err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Message, "Failed to pull image") {
			found = true
		}
		if strings.Contains(e.Message, "OOMKilled") || strings.Contains(e.Message, "OOMKilling") {
			t.Fatalf("镜像场景不应残留 OOM 事件，得到 %q", e.Message)
		}
	}
	if !found {
		t.Fatalf("镜像场景应有 Failed to pull image 事件，得到 %+v", events)
	}
	if !strings.Contains(f.Logs[DemoPod], "Failed to pull image") {
		t.Fatalf("镜像场景日志应包含 Failed to pull image")
	}
	dep, err := f.GetDeployment(context.Background(), DemoNamespace, DemoDeployment)
	if err != nil {
		t.Fatalf("取 Deployment 失败: %v", err)
	}
	if dep.Spec.Template.Spec.Containers[0].Image != "demo-api:bad-tag" {
		t.Fatalf("镜像场景 Deployment 的镜像应为 demo-api:bad-tag，得到 %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestNewFakeWithScenarioProbe(t *testing.T) {
	f := NewFakeWithScenario("probe")
	pod, err := f.GetPod(context.Background(), DemoNamespace, DemoPod)
	if err != nil {
		t.Fatalf("取 Pod 失败: %v", err)
	}
	if len(pod.Status.ContainerStatuses) == 0 || pod.Status.ContainerStatuses[0].RestartCount != 3 {
		t.Fatalf("探针场景重启次数应为 3，得到 %+v", pod.Status.ContainerStatuses)
	}
	events, err := f.ListEvents(context.Background(), DemoNamespace)
	if err != nil {
		t.Fatalf("取 Events 失败: %v", err)
	}
	found := false
	for _, e := range events {
		if strings.Contains(e.Message, "Liveness probe failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("探针场景应有 Liveness probe failed 事件，得到 %+v", events)
	}
	if !strings.Contains(f.Logs[DemoPod], "liveness probe failed") {
		t.Fatalf("探针场景日志应包含 liveness probe failed")
	}
}

func TestNewFakeDefaultIsOOM(t *testing.T) {
	f := NewFake()
	if f.Scenario != "oom" {
		t.Fatalf("默认场景应为 oom，得到 %q", f.Scenario)
	}
}
