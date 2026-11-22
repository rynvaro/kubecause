package mcpclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kubecause/internal/node"
)

// startTestClient 用内存传输把 node.Server 和 Client 接起来，
// 返回 Client；调用方用完要 Close（t.Cleanup 里已处理）。
// 这是“真的走了 MCP 协议”的验证：工具发现和调用都过 JSON-RPC，
// 不是进程内直接调 executor。
func startTestClient(t *testing.T, exec node.NodeExecutor) *Client {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := node.NewServer(exec)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect 失败: %v", err)
	}
	c, err := Connect(ctx, clientTransport)
	if err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		serverSession.Wait()
	})
	return c
}

func TestClientDiscoversNodeTools(t *testing.T) {
	c := startTestClient(t, &node.FakeExecutor{Output: "ok"})
	want := map[string]bool{"node_list": true, "node_disk": true, "node_memory": true, "node_processes": true, "node_logs": true}
	if len(c.ToolDefs()) != len(want) {
		t.Fatalf("发现 %d 个工具, want %d: %v", len(c.ToolDefs()), len(want), c.Names())
	}
	for _, def := range c.ToolDefs() {
		if !want[def.Name] {
			t.Errorf("发现了意外的工具 %q", def.Name)
		}
		if def.Description == "" {
			t.Errorf("工具 %q 没有描述", def.Name)
		}
		if def.Parameters == nil {
			t.Errorf("工具 %q 没有 Parameters", def.Name)
		}
	}
	// node_disk 的 schema 里 node 必须是必填的——
	// 模型看到的工具定义就是这份，发现错了模型就会填错参数。
	var diskDef *ToolDef
	for i := range c.ToolDefs() {
		if c.ToolDefs()[i].Name == "node_disk" {
			diskDef = &c.ToolDefs()[i]
		}
	}
	if diskDef == nil {
		t.Fatalf("没有发现 node_disk")
	}
	required, _ := diskDef.Parameters["required"].([]any)
	found := false
	for _, r := range required {
		if r == "node" {
			found = true
		}
	}
	if !found {
		t.Errorf("node_disk 的 schema.required = %v, 应该包含 node", required)
	}
	if !c.HasTool("node_disk") || c.HasTool("no_such_tool") {
		t.Errorf("HasTool 判断不对：node_disk 应该有，no_such_tool 应该没有")
	}
}

func TestClientCallToolGoesThroughMCP(t *testing.T) {
	// 关键验证：模型调 node_disk 时，调用真的经过 MCP 会话
	// 到达 Server 的 FakeExecutor，而不是进程内直接调执行器。
	// fake.Calls 里有记录就是证据——进程内调用不会经过这里。
	fake := &node.FakeExecutor{Output: "fake-disk-output"}
	c := startTestClient(t, fake)
	out, err := c.CallTool(context.Background(), "node_disk", map[string]string{"node": "kind-control-plane"})
	if err != nil {
		t.Fatalf("CallTool 失败: %v", err)
	}
	if out.Summary != "fake-disk-output" {
		t.Errorf("CallTool 摘要 = %q, want %q", out.Summary, "fake-disk-output")
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Node != "kind-control-plane" {
		t.Fatalf("fake 调用记录 = %+v, want 1 次、节点 kind-control-plane", fake.Calls)
	}
	if got := strings.Join(fake.Calls[0].Command, " "); got != "chroot /host df -h" {
		t.Errorf("Server 侧执行的命令 = %q, want chroot /host df -h", got)
	}
}

func TestClientCallToolNodeList(t *testing.T) {
	fake := &node.FakeExecutor{Nodes: []node.NodeInfo{
		{Name: "kind-control-plane", Ready: true},
	}}
	c := startTestClient(t, fake)
	out, err := c.CallTool(context.Background(), "node_list", map[string]string{})
	if err != nil {
		t.Fatalf("CallTool node_list 失败: %v", err)
	}
	if out.Summary != "kind-control-plane Ready" {
		t.Errorf("node_list 摘要 = %q, want %q", out.Summary, "kind-control-plane Ready")
	}
}

func TestClientCallToolError(t *testing.T) {
	// Server 侧执行失败（IsError 结果）时，Client 返回 error，
	// Agent 会记一步失败、模型下一轮能看到失败原因，而不是悄悄吞掉。
	fake := &node.FakeExecutor{Err: errors.New("connection refused")}
	c := startTestClient(t, fake)
	_, err := c.CallTool(context.Background(), "node_disk", map[string]string{"node": "kind-control-plane"})
	if err == nil {
		t.Fatalf("Server 报错时 CallTool 应该返回 error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("CallTool 错误 = %q, 应该包含 Server 的报错", err.Error())
	}
}

func TestClientCallToolUnknown(t *testing.T) {
	c := startTestClient(t, &node.FakeExecutor{Output: "ok"})
	_, err := c.CallTool(context.Background(), "no_such_tool", map[string]string{})
	if err == nil {
		t.Fatalf("调没发现的工具应该返回 error")
	}
}

func TestStartMissingBinary(t *testing.T) {
	// 降级路径的验证：mcp-server 二进制找不到时 Start 必须返回错误、
	// 不能挂住，main.go 靠这个错误决定降级。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Start(ctx, "/nonexistent/mcp-server")
	if err == nil {
		t.Fatalf("二进制不存在时 Start 应该返回 error")
	}
	t.Logf("Start 失败时的错误: %v", err)
}
