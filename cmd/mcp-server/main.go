// Command mcp-server 是 KubeCause 的 Linux 节点 MCP Server：
// 独立进程，走 stdio 和 MCP 客户端（inspector、Claude Desktop 等）通信，
// 提供 4 个只读的节点诊断工具。
//
// 启动：
//
//	KUBECONFIG=~/.kube/config go run ./cmd/mcp-server
//
// 日志只打到 stderr，stdout 是 MCP 协议通道，绝不能混进其他输出。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kubecause/internal/node"
)

func main() {
	log.SetOutput(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	exec, err := node.NewDebugPodExecutor(os.Getenv("KUBECONFIG"))
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	server := node.NewServer(exec)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("MCP 服务退出: %v", err)
	}
}
