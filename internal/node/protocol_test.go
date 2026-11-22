package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// unreachableKubeconfig 写一个指向连不上地址的 kubeconfig，
// 模拟“VM 里没有集群”的情况：建客户端本身不连 API Server，
// 第一次工具调用才会在查节点时报连接错误。
func unreachableKubeconfig(t *testing.T) string {
	t.Helper()
	const cfg = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
    insecure-skip-tls-verify: true
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test
`
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(p, []byte(cfg), 0600); err != nil {
		t.Fatalf("写测试 kubeconfig 失败: %v", err)
	}
	return p
}

// startTestServer 用内存传输把 server 和 client 接起来，
// 返回 client 会话；调用方用完要 Close。
func startTestServer(t *testing.T, exec NodeExecutor) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := NewServer(exec)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect 失败: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect 失败: %v", err)
	}
	t.Cleanup(func() {
		clientSession.Close()
		serverSession.Wait()
	})
	return clientSession
}

func TestProtocolListTools(t *testing.T) {
	ctx := context.Background()
	cs := startTestServer(t, &FakeExecutor{Output: "ok"})
	res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools 失败: %v", err)
	}
	want := map[string]bool{"node_disk": true, "node_memory": true, "node_processes": true, "node_logs": true}
	if len(res.Tools) != len(want) {
		t.Fatalf("ListTools 返回 %d 个工具, want %d: %v", len(res.Tools), len(want), res.Tools)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
		if tool.Description == "" {
			t.Errorf("工具 %q 没有描述", tool.Name)
		}
	}
	for name := range want {
		if _, ok := byName[name]; !ok {
			t.Errorf("ListTools 缺少工具 %q", name)
		}
	}
	// node_disk 的入参里 node 必须是必填的。
	schema, ok := byName["node_disk"].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("node_disk 的 InputSchema 类型 = %T, want map[string]any", byName["node_disk"].InputSchema)
	}
	required, _ := schema["required"].([]any)
	found := false
	for _, r := range required {
		if r == "node" {
			found = true
		}
	}
	if !found {
		t.Errorf("node_disk 的 inputSchema.required = %v, 应该包含 node", required)
	}
	// node_logs 的 lines 是可选的。
	logsSchema, ok := byName["node_logs"].InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("node_logs 的 InputSchema 类型 = %T, want map[string]any", byName["node_logs"].InputSchema)
	}
	required, _ = logsSchema["required"].([]any)
	for _, r := range required {
		if r == "lines" {
			t.Errorf("node_logs 的 lines 不该是必填的: required = %v", required)
		}
	}
}

func TestProtocolCallToolFake(t *testing.T) {
	ctx := context.Background()
	fake := &FakeExecutor{Output: "fake-disk-output"}
	cs := startTestServer(t, fake)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "node_disk",
		Arguments: map[string]any{"node": "kind-control-plane"},
	})
	if err != nil {
		t.Fatalf("CallTool 失败: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool 返回 IsError: %v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] 类型 = %T, want *mcp.TextContent", res.Content[0])
	}
	if text.Text != "fake-disk-output" {
		t.Errorf("CallTool 文本 = %q, want %q", text.Text, "fake-disk-output")
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Node != "kind-control-plane" {
		t.Fatalf("fake 调用记录 = %+v, want 1 次、节点 kind-control-plane", fake.Calls)
	}
}

func TestProtocolCallToolNoCluster(t *testing.T) {
	// VM 里没有集群：server 用真实执行器 + 连不上的 kubeconfig，
	// tools/call 必须优雅失败——返回 IsError 结果、报错说清楚，
	// 而不是协议级错误，更不能崩。
	ctx := context.Background()
	exec, err := NewDebugPodExecutor(unreachableKubeconfig(t))
	if err != nil {
		t.Fatalf("NewDebugPodExecutor 失败: %v", err)
	}
	cs := startTestServer(t, exec)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "node_disk",
		Arguments: map[string]any{"node": "kind-control-plane"},
	})
	if err != nil {
		t.Fatalf("CallTool 返回协议错误: %v（应该返回 IsError 结果，不是协议错误）", err)
	}
	if !res.IsError {
		t.Fatalf("没有集群时 IsError = false, want true")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] 类型 = %T, want *mcp.TextContent", res.Content[0])
	}
	// 报错要说清楚是连不上集群，而不是含糊的失败。
	if !strings.Contains(text.Text, "kind-control-plane") {
		t.Errorf("报错文本 = %q, 应该提到节点名", text.Text)
	}
	t.Logf("无集群时的报错文本: %s", text.Text)
}
