// Package mcpclient 是 KubeCause 主程序里的 MCP 客户端：
// 把 mcp-server 子进程拉起来，走 stdio 用 MCP 协议调它的节点工具。
//
// 它只做协议和发现：启动子进程、initialize、tools/list 拿到工具定义、
// tools/call 转发调用。工具该不该出现、调谁，由 model / agent 决定，
// 这里不替模型做决策。
//
// 主程序启动失败时（子进程拉不起来、initialize 失败），
// Start 返回错误，调用方（main.go）降级：没有节点工具，
// 诊断照常跑——这层绝不能让主流程挂掉。
package mcpclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"kubecause/internal/agent"
)

// ToolDef 是从 MCP Server 发现的一个工具的定义，
// 和模型发给大模型的工具定义同形（名字、描述、JSON Schema 参数）。
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Client 是一次 MCP 会话：背后是 mcp-server 子进程（Start 拉起来的）
// 或测试里的内存传输（Connect 接进来的）。
type Client struct {
	session *mcp.ClientSession
	tools   []ToolDef
	byName  map[string]bool
}

// Start 拉起 mcp-server 子进程并完成 initialize + tools/list，
// 拿到它的工具定义。任何一步失败都返回错误，
// 子进程已尽力清理，调用方据此降级，不要重试、不要让主流程挂掉。
func Start(ctx context.Context, binPath string) (*Client, error) {
	cmd := exec.Command(binPath)
	// 子进程的 stderr 透到主进程的 stderr：mcp-server 的日志本来就只打 stderr，
	// stdout 是协议通道，不能混。接通失败时主程序日志里能看到原因。
	cmd.Stderr = os.Stderr
	c, err := Connect(ctx, &mcp.CommandTransport{Command: cmd})
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, err
	}
	return c, nil
}

// Connect 在一条已有的传输上完成 initialize + tools/list，
// 拿到 Server 的工具定义。Start 是它的子进程版本；
// 测试用内存传输（mcp.NewInMemoryTransports）调这个，不用起真子进程。
func Connect(ctx context.Context, transport mcp.Transport) (*Client, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "kubecause-agent", Version: "v0.14.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 MCP Server 失败: %w", err)
	}
	c := &Client{session: session, byName: map[string]bool{}}
	if err := c.discover(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	return c, nil
}

// discover 调 tools/list，把 Server 的工具定义转成 ToolDef。
// InputSchema 在客户端侧是 map[string]any，直接当 Parameters 用，
// 模型发给大模型的就是这份 schema。
func (c *Client) discover(ctx context.Context) error {
	res, err := c.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("MCP tools/list 失败: %w", err)
	}
	for _, t := range res.Tools {
		def := ToolDef{Name: t.Name, Description: t.Description}
		if schema, ok := t.InputSchema.(map[string]any); ok {
			def.Parameters = schema
		} else {
			def.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		c.tools = append(c.tools, def)
		c.byName[t.Name] = true
	}
	if len(c.tools) == 0 {
		return fmt.Errorf("MCP Server 没有提供任何工具")
	}
	return nil
}

// ToolDefs 返回发现的工具定义（发现时的顺序），main.go 转给模型用。
func (c *Client) ToolDefs() []ToolDef {
	return c.tools
}

// HasTool 判断 Server 是否提供了这个工具，分发时用它决定走不走 MCP。
func (c *Client) HasTool(name string) bool {
	return c.byName[name]
}

// Names 返回发现的工具名，main.go 注册进 agent 的校验白名单、打启动日志用。
func (c *Client) Names() []string {
	out := make([]string, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, t.Name)
	}
	return out
}

// CallTool 调 Server 上的一个工具，返回给模型看的摘要和结构化数据。
// 参数是模型给的字符串表（agent.ToolCall.Args）：
// node_logs 的 lines 在 MCP schema 里是数字，这里转成数字再发，
// 字符串发过去会被 Server 的 schema 校验拒绝。
// Server 返回 IsError 时这里返回 error，Agent 会记一步失败、
// 模型下一轮能看到失败原因、换个办法重试，而不是悄悄吞掉。
func (c *Client) CallTool(ctx context.Context, name string, args map[string]string) (agent.ToolOutput, error) {
	if !c.byName[name] {
		return agent.ToolOutput{}, fmt.Errorf("MCP Server 没有提供工具 %q", name)
	}
	mcpArgs := make(map[string]any, len(args))
	for k, v := range args {
		mcpArgs[k] = v
	}
	if v, ok := mcpArgs["lines"]; ok {
		if s, ok := v.(string); ok {
			n, err := strconv.Atoi(s)
			if err != nil {
				return agent.ToolOutput{}, fmt.Errorf("lines 参数 %q 不是数字", s)
			}
			mcpArgs["lines"] = n
		}
	}
	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: mcpArgs})
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("调 MCP 工具 %s 失败: %w", name, err)
	}
	text := textContent(res)
	if res.IsError {
		return agent.ToolOutput{}, fmt.Errorf("MCP 工具 %s 返回错误: %s", name, text)
	}
	return agent.ToolOutput{Summary: text, Data: map[string]any{"text": text}}, nil
}

// textContent 把 CallTool 结果里的文本内容拼成一段文本。
// 节点工具只返回文本，没有结构化数据，Data 里也只放这段文本。
func textContent(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Close 关闭 MCP 会话：关掉 stdio，子进程随之退出
// （CommandTransport 的 Close 会等进程退出、超时再 SIGTERM/SIGKILL）。
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}
