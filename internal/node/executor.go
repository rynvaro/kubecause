// Package node 是 Linux 节点 MCP Server 的实现。
//
// 它和 KubeCause 主程序（Kubernetes 故障诊断 Agent）是分开的进程：
// 这里只做一件事——用 kubectl debug 的思路（在目标节点上起一个
// hostPID 调试 Pod，用 chroot 进宿主机）跑 4 条只读命令，
// 把结果以 MCP 工具的形式交给模型。
//
// 传输层和工具层是分开的：NodeExecutor 是执行命令的接口，
// tools.go 只负责把 MCP 工具调用翻译成一条固定命令；
// 测试用 fakeExecutor，不需要真实集群。
package node

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NodeInfo 是一个节点的一行摘要：名字和 Ready 状态。
// node_list 只返回这两列，不返回节点详情——
// 模型查节点工具之前只需要知道“有哪些节点、哪个能用”，
// 详情（磁盘、内存、日志）由另外 4 个工具按节点名去查。
type NodeInfo struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// NodeExecutor 在指定节点上执行一条命令，返回合并后的 stdout/stderr。
// command 是 exec 的 argv（不经过 shell），调用方必须传固定命令，
// 绝不能把用户输入拼进 shell。
type NodeExecutor interface {
	ExecOnNode(ctx context.Context, node string, command []string) (string, error)
	// ListNodes 列出集群的全部节点（名字 + Ready 状态），
	// 给模型一个拿到节点名的渠道：KubeCause 的 k8s 工具
	// （get_pod、list_pods、get_cluster_overview）都不返回节点名，
	// 没有这个工具，模型连 node_disk 的 node 参数都填不出来。
	ListNodes(ctx context.Context) ([]NodeInfo, error)
}

// nodeNameRe 是 Kubernetes 节点名的规则（DNS 子域名，小写字母、数字、-、.）：
// 校验失败时直接拒绝，带 shell 元字符（; | $ ` 空格 / 等）的参数到不了执行层。
var nodeNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// ValidateNodeName 校验节点名是否符合 Kubernetes 名字规则。
func ValidateNodeName(node string) error {
	if node == "" {
		return fmt.Errorf("节点名不能为空")
	}
	if len(node) > 253 {
		return fmt.Errorf("节点名过长（%d 字符，上限 253）", len(node))
	}
	if !nodeNameRe.MatchString(node) {
		return fmt.Errorf("节点名 %q 不合法：只允许小写字母、数字、- 和 .", node)
	}
	return nil
}

const (
	// DefaultLogLines 是 node_logs 不传 lines 时的默认值。
	DefaultLogLines = 100
	// MaxLogLines 是 node_logs 的 lines 上限。
	MaxLogLines = 500
	// MaxProcessLines 是 node_processes 返回结果保留的最大行数。
	MaxProcessLines = 20
)

// ClampLogLines 把 lines 规范到 [1, MaxLogLines]，0 或负数表示用默认值。
func ClampLogLines(lines int) int {
	if lines <= 0 {
		return DefaultLogLines
	}
	if lines > MaxLogLines {
		return MaxLogLines
	}
	return lines
}

// DiskCommand 返回 node_disk 的固定命令：宿主机磁盘用量。
func DiskCommand() []string {
	return []string{"chroot", "/host", "df", "-h"}
}

// MemoryCommand 返回 node_memory 的固定命令：宿主机内存用量。
func MemoryCommand() []string {
	return []string{"chroot", "/host", "free", "-h"}
}

// ProcessesCommand 返回 node_processes 的固定命令：按内存占用排序的进程列表。
// 输出在 Go 代码里截前 MaxProcessLines 行（见 TruncateLines），
// 这里不拼管道、不走 sh -c。
func ProcessesCommand() []string {
	return []string{"chroot", "/host", "ps", "aux", "--sort=-%mem"}
}

// LogsCommand 返回 node_logs 的固定命令：kubelet 最近 lines 行日志。
func LogsCommand(lines int) []string {
	return []string{"chroot", "/host", "journalctl", "-u", "kubelet", "--no-pager", "-n", strconv.Itoa(ClampLogLines(lines))}
}

// TruncateLines 只保留输出的前 n 行，多余的丢掉。
// n <= 0 时返回空字符串；行数不够 n 时原样返回。
func TruncateLines(out string, n int) string {
	if n <= 0 {
		return ""
	}
	trimmed := strings.TrimRight(out, "\n")
	if trimmed == "" {
		return out
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= n {
		return out
	}
	return strings.Join(lines[:n], "\n") + "\n"
}

// FakeCall 是一次 ExecOnNode 调用的记录。
type FakeCall struct {
	Node    string
	Command []string
}

// FakeExecutor 是 NodeExecutor 的测试实现：
// 记录每次调用的节点和命令，按预设的输出返回，不碰真实集群。
type FakeExecutor struct {
	// Output 是每次调用返回的输出。
	Output string
	// Err 是每次调用返回的错误，设置后 ExecOnNode 直接返回它。
	Err error
	// Nodes 是 ListNodes 返回的节点列表。
	Nodes []NodeInfo
	// Calls 记录每次调用的节点和命令。
	Calls []FakeCall
}

// ExecOnNode 记录调用并返回预设的输出或错误。
func (f *FakeExecutor) ExecOnNode(_ context.Context, node string, command []string) (string, error) {
	cp := make([]string, len(command))
	copy(cp, command)
	f.Calls = append(f.Calls, FakeCall{Node: node, Command: cp})
	if f.Err != nil {
		return "", f.Err
	}
	return f.Output, nil
}

// ListNodes 返回预设的节点列表，不碰真实集群。
func (f *FakeExecutor) ListNodes(_ context.Context) ([]NodeInfo, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Nodes, nil
}
