package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiskCommand(t *testing.T) {
	want := []string{"chroot", "/host", "df", "-h"}
	got := DiskCommand()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("DiskCommand() = %v, want %v", got, want)
	}
}

func TestMemoryCommand(t *testing.T) {
	want := []string{"chroot", "/host", "free", "-h"}
	got := MemoryCommand()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("MemoryCommand() = %v, want %v", got, want)
	}
}

func TestProcessesCommand(t *testing.T) {
	// 不能拼管道、不能走 sh -c：命令必须是干净的 argv。
	want := []string{"chroot", "/host", "ps", "aux", "--sort=-%mem"}
	got := ProcessesCommand()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ProcessesCommand() = %v, want %v", got, want)
	}
	for _, arg := range got {
		if strings.ContainsAny(arg, "|;&$`") {
			t.Fatalf("ProcessesCommand() 里出现 shell 元字符: %q", arg)
		}
	}
}

func TestLogsCommand(t *testing.T) {
	cases := []struct {
		lines int
		want  string
	}{
		{0, "100"},    // 不传：默认值
		{-5, "100"},   // 负数：默认值
		{1, "1"},      // 下限
		{50, "50"},    // 正常值
		{500, "500"},  // 上限
		{501, "500"},  // 超上限：截到 500
		{1000, "500"}, // 远超上限：截到 500
	}
	for _, c := range cases {
		got := LogsCommand(c.lines)
		want := []string{"chroot", "/host", "journalctl", "-u", "kubelet", "--no-pager", "-n", c.want}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("LogsCommand(%d) = %v, want %v", c.lines, got, want)
		}
	}
}

func TestClampLogLines(t *testing.T) {
	if got := ClampLogLines(0); got != DefaultLogLines {
		t.Errorf("ClampLogLines(0) = %d, want %d", got, DefaultLogLines)
	}
	if got := ClampLogLines(600); got != MaxLogLines {
		t.Errorf("ClampLogLines(600) = %d, want %d", got, MaxLogLines)
	}
	if got := ClampLogLines(42); got != 42 {
		t.Errorf("ClampLogLines(42) = %d, want 42", got)
	}
}

func TestTruncateLines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	got := TruncateLines(b.String(), 20)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("TruncateLines(25 行, 20) = %d 行, want 20", len(lines))
	}
	if lines[0] != "line 1" || lines[19] != "line 20" {
		t.Fatalf("TruncateLines 保留的不是前 20 行: 首行 %q, 末行 %q", lines[0], lines[19])
	}
	// 行数不够时原样返回。
	short := "a\nb\n"
	if got := TruncateLines(short, 20); got != short {
		t.Fatalf("TruncateLines(2 行, 20) = %q, want 原样 %q", got, short)
	}
}

func TestValidateNodeName(t *testing.T) {
	valid := []string{"node-1", "kind-control-plane", "a", "a.b-c", "worker-0"}
	for _, n := range valid {
		if err := ValidateNodeName(n); err != nil {
			t.Errorf("ValidateNodeName(%q) = %v, want nil", n, err)
		}
	}
	// 带 shell 元字符的必须拒绝，绝不能让参数拼进 shell。
	invalid := []string{
		"", "A", "Node-1", "-a", "a-", "a b", "a;b", "a|b", "a&b",
		"$(id)", "`id`", "a/b", "a_b", "a:b", "a\tb", "a\nb",
	}
	for _, n := range invalid {
		if err := ValidateNodeName(n); err == nil {
			t.Errorf("ValidateNodeName(%q) = nil, want error", n)
		}
	}
}

func textResult(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		t.Fatal("结果为空")
	}
	if res.IsError {
		t.Fatalf("结果是 IsError，不该是：%v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("Content 长度 = %d, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] 类型 = %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

func TestHandlersWithFakeExecutor(t *testing.T) {
	ctx := context.Background()
	fake := &FakeExecutor{Output: "fake-output"}
	s := &Server{exec: fake}

	// node_disk：拼出的命令对不对、返回执行器输出对不对。
	res, _, err := s.nodeDisk(ctx, nil, NodeInput{Node: "kind-control-plane"})
	if err != nil {
		t.Fatalf("nodeDisk 返回协议错误: %v", err)
	}
	if got := textResult(t, res); got != "fake-output" {
		t.Errorf("nodeDisk 文本 = %q, want %q", got, "fake-output")
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Node != "kind-control-plane" {
		t.Fatalf("nodeDisk 调用记录 = %+v, want 1 次、节点 kind-control-plane", fake.Calls)
	}
	if fmt.Sprint(fake.Calls[0].Command) != fmt.Sprint(DiskCommand()) {
		t.Errorf("nodeDisk 命令 = %v, want %v", fake.Calls[0].Command, DiskCommand())
	}

	// node_memory。
	fake.Calls = nil
	res, _, err = s.nodeMemory(ctx, nil, NodeInput{Node: "kind-control-plane"})
	if err != nil {
		t.Fatalf("nodeMemory 返回协议错误: %v", err)
	}
	textResult(t, res)
	if fmt.Sprint(fake.Calls[0].Command) != fmt.Sprint(MemoryCommand()) {
		t.Errorf("nodeMemory 命令 = %v, want %v", fake.Calls[0].Command, MemoryCommand())
	}

	// node_processes：25 行输出截到 20 行。
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "proc %d\n", i)
	}
	fake.Output = b.String()
	fake.Calls = nil
	res, _, err = s.nodeProcesses(ctx, nil, NodeInput{Node: "kind-control-plane"})
	if err != nil {
		t.Fatalf("nodeProcesses 返回协议错误: %v", err)
	}
	got := textResult(t, res)
	if lines := strings.Split(strings.TrimRight(got, "\n"), "\n"); len(lines) != 20 {
		t.Errorf("nodeProcesses 返回 %d 行, want 20", len(lines))
	}
	if fmt.Sprint(fake.Calls[0].Command) != fmt.Sprint(ProcessesCommand()) {
		t.Errorf("nodeProcesses 命令 = %v, want %v", fake.Calls[0].Command, ProcessesCommand())
	}

	// node_logs：lines 600 要截到 500。
	fake.Output = "log-line"
	fake.Calls = nil
	res, _, err = s.nodeLogs(ctx, nil, LogsInput{Node: "kind-control-plane", Lines: 600})
	if err != nil {
		t.Fatalf("nodeLogs 返回协议错误: %v", err)
	}
	textResult(t, res)
	if fmt.Sprint(fake.Calls[0].Command) != fmt.Sprint(LogsCommand(500)) {
		t.Errorf("nodeLogs 命令 = %v, want %v", fake.Calls[0].Command, LogsCommand(500))
	}
}

func TestHandlerRejectsBadNode(t *testing.T) {
	ctx := context.Background()
	fake := &FakeExecutor{Output: "x"}
	s := &Server{exec: fake}
	// 参数里塞 shell 元字符：必须拒绝，且执行器一次都不能被调到。
	res, _, err := s.nodeDisk(ctx, nil, NodeInput{Node: "kind-control-plane; rm -rf /"})
	if err != nil {
		t.Fatalf("nodeDisk 返回协议错误: %v（应该返回 IsError 结果，不是协议错误）", err)
	}
	if !res.IsError {
		t.Errorf("nodeDisk(带 shell 元字符的节点名) IsError = false, want true")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("非法节点名还调到了执行器: %+v", fake.Calls)
	}
}

func TestHandlerExecutorErrorIsGraceful(t *testing.T) {
	ctx := context.Background()
	fake := &FakeExecutor{Err: errors.New("connection refused")}
	s := &Server{exec: fake}
	res, _, err := s.nodeDisk(ctx, nil, NodeInput{Node: "kind-control-plane"})
	if err != nil {
		t.Fatalf("nodeDisk 返回协议错误: %v（应该返回 IsError 结果，不是协议错误）", err)
	}
	if !res.IsError {
		t.Errorf("执行器报错时 IsError = false, want true")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] 类型 = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "connection refused") {
		t.Errorf("报错文本 = %q, 应该包含底层错误原因", text.Text)
	}
}

func TestDebugPodName(t *testing.T) {
	name := debugPodName("kind-control-plane")
	if len(name) > 63 {
		t.Errorf("debugPodName 长度 = %d, want <= 63", len(name))
	}
	if !strings.HasPrefix(name, "node-debugger-kind-control-plane-") {
		t.Errorf("debugPodName = %q, want 前缀 node-debugger-kind-control-plane-", name)
	}
	// 节点名很长时也要保证总长不超过 63。
	long := strings.Repeat("a", 100)
	name = debugPodName(long)
	if len(name) > 63 {
		t.Errorf("debugPodName(100 字符节点名) 长度 = %d, want <= 63", len(name))
	}
	if !strings.HasPrefix(name, "node-debugger-") {
		t.Errorf("debugPodName(100 字符节点名) = %q, want 前缀 node-debugger-", name)
	}
	// 两次生成的后缀应该不同（随机）。
	if debugPodName("n") == debugPodName("n") {
		t.Errorf("debugPodName 两次生成相同，可能随机后缀没起作用")
	}
}

func TestDebugPodSpec(t *testing.T) {
	pod := debugPod("node-debugger-n-abcdef", "kind-control-plane", "busybox:latest")
	if pod.Namespace != "default" {
		t.Errorf("调试 Pod namespace = %q, want default", pod.Namespace)
	}
	if pod.Spec.NodeName != "kind-control-plane" {
		t.Errorf("调试 Pod nodeName = %q, want kind-control-plane", pod.Spec.NodeName)
	}
	if !pod.Spec.HostPID {
		t.Errorf("调试 Pod hostPID = false, want true")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("调试 Pod 容器数 = %d, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "busybox:latest" {
		t.Errorf("调试 Pod 镜像 = %q, want busybox:latest", c.Image)
	}
	if fmt.Sprint(c.Command) != "[sleep infinity]" {
		t.Errorf("调试 Pod 命令 = %v, want [sleep infinity]", c.Command)
	}
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Errorf("调试 Pod 容器不是 privileged")
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/host" {
		t.Errorf("调试 Pod 挂载 = %+v, want 挂载 /host", c.VolumeMounts)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].HostPath == nil || pod.Spec.Volumes[0].HostPath.Path != "/" {
		t.Errorf("调试 Pod 卷 = %+v, want hostPath /", pod.Spec.Volumes)
	}
	if pod.Spec.RestartPolicy != "Never" {
		t.Errorf("调试 Pod 重启策略 = %q, want Never", pod.Spec.RestartPolicy)
	}
}

func TestDebugImageDefault(t *testing.T) {
	t.Setenv("MCP_DEBUG_IMAGE", "")
	if got := debugImage(); got != "busybox:latest" {
		t.Errorf("debugImage() 默认 = %q, want busybox:latest", got)
	}
	t.Setenv("MCP_DEBUG_IMAGE", "ubuntu:22.04")
	if got := debugImage(); got != "ubuntu:22.04" {
		t.Errorf("debugImage() 环境变量覆盖 = %q, want ubuntu:22.04", got)
	}
}
