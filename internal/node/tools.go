package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NodeInput 是 node_disk / node_memory / node_processes 的入参：只要节点名。
type NodeInput struct {
	Node string `json:"node" jsonschema:"要查询的 Kubernetes 节点名"`
}

// LogsInput 是 node_logs 的入参：节点名 + 可选的行数。
type LogsInput struct {
	Node  string `json:"node" jsonschema:"要查询的 Kubernetes 节点名"`
	Lines int    `json:"lines,omitempty" jsonschema:"返回最近多少行日志，默认 100，上限 500"`
}

// Server 是 MCP 工具的持有者：把 MCP 工具调用翻译成一条固定命令，
// 交给 NodeExecutor 执行。Server 本身不碰集群，执行细节全在 executor 里，
// 测试时换一个 fake executor 就能测。
type Server struct {
	exec NodeExecutor
}

// EmptyInput 是 node_list 的入参：不需要参数。
type EmptyInput struct{}

// NewServer 组装一个 MCP server，注册 5 个只读工具。
// 工具描述只写返回什么和覆盖范围。
func NewServer(exec NodeExecutor) *mcp.Server {
	s := &Server{exec: exec}
	server := mcp.NewServer(&mcp.Implementation{Name: "kubecause-linux", Version: "v0.14.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "node_list",
		Description: "列出集群的全部节点，返回节点名和 Ready 状态；查节点磁盘、内存或日志之前，先用它拿到节点名。",
	}, s.nodeList)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "node_disk",
		Description: "查看指定节点的磁盘用量：返回 chroot 进宿主机根目录后执行 df -h 的输出，每行一个挂载点，含总容量、已用、可用和使用率。",
	}, s.nodeDisk)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "node_memory",
		Description: "查看指定节点的内存用量：返回 chroot 进宿主机根目录后执行 free -h 的输出，含物理内存和交换分区。",
	}, s.nodeMemory)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "node_processes",
		Description: "查看指定节点上按内存占用排序的进程列表：返回 chroot 进宿主机根目录后执行 ps aux --sort=-%mem 的输出，只保留前 20 行。",
	}, s.nodeProcesses)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "node_logs",
		Description: "查看指定节点上 kubelet 最近的日志：返回 chroot 进宿主机根目录后执行 journalctl -u kubelet --no-pager 的输出，lines 指定行数，默认 100，上限 500。",
	}, s.nodeLogs)
	return server
}

// nodeList 列出节点：每行一个节点，“名字 Ready/NotReady”，没有节点时说清楚。
func (s *Server) nodeList(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, any, error) {
	nodes, err := s.exec.ListNodes(ctx)
	if err != nil {
		res := &mcp.CallToolResult{}
		res.SetError(fmt.Errorf("列出节点失败: %w", err))
		return res, nil, nil
	}
	if len(nodes) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "集群里没有节点"}},
		}, nil, nil
	}
	var b strings.Builder
	for _, n := range nodes {
		status := "NotReady"
		if n.Ready {
			status = "Ready"
		}
		fmt.Fprintf(&b, "%s %s\n", n.Name, status)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}},
	}, nil, nil
}

func (s *Server) nodeDisk(ctx context.Context, _ *mcp.CallToolRequest, in NodeInput) (*mcp.CallToolResult, any, error) {
	return s.run(ctx, in.Node, DiskCommand(), 0)
}

func (s *Server) nodeMemory(ctx context.Context, _ *mcp.CallToolRequest, in NodeInput) (*mcp.CallToolResult, any, error) {
	return s.run(ctx, in.Node, MemoryCommand(), 0)
}

func (s *Server) nodeProcesses(ctx context.Context, _ *mcp.CallToolRequest, in NodeInput) (*mcp.CallToolResult, any, error) {
	return s.run(ctx, in.Node, ProcessesCommand(), MaxProcessLines)
}

func (s *Server) nodeLogs(ctx context.Context, _ *mcp.CallToolRequest, in LogsInput) (*mcp.CallToolResult, any, error) {
	return s.run(ctx, in.Node, LogsCommand(in.Lines), 0)
}

// run 是 4 个工具的公共路径：校验节点名 → 执行固定命令 →
// 截断（只要 maxLines > 0）→ 文本结果。
// 执行失败时返回 IsError 的结果、报错说清楚，不走协议级错误，
// 客户端能正常收到失败原因。
func (s *Server) run(ctx context.Context, node string, command []string, maxLines int) (*mcp.CallToolResult, any, error) {
	if err := ValidateNodeName(node); err != nil {
		res := &mcp.CallToolResult{}
		res.SetError(err)
		return res, nil, nil
	}
	out, err := s.exec.ExecOnNode(ctx, node, command)
	if err != nil {
		res := &mcp.CallToolResult{}
		res.SetError(fmt.Errorf("在节点 %q 上执行失败: %w", node, err))
		return res, nil, nil
	}
	if maxLines > 0 {
		out = TruncateLines(out, maxLines)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: out}},
	}, nil, nil
}
