package agent

import (
	"context"
	"strings"
	"testing"
)

// fakeNodeTools 是 NodeTools 的测试替身：记录调用，返回预设输出。
// 它证明分发真的走了 MCP 接口——调用记录在这里，不在 ToolsAdapter 里。
type fakeNodeTools struct {
	tools  map[string]bool
	output string
	err    error
	calls  []string
}

func (f *fakeNodeTools) HasTool(name string) bool { return f.tools[name] }

func (f *fakeNodeTools) CallTool(_ context.Context, name string, args map[string]string) (ToolOutput, error) {
	f.calls = append(f.calls, name+" node="+args["node"])
	if f.err != nil {
		return ToolOutput{}, f.err
	}
	return ToolOutput{Summary: f.output, Data: map[string]any{"text": f.output}}, nil
}

func TestValidateNodeToolCall(t *testing.T) {
	RegisterNodeTools([]string{"node_list", "node_disk", "node_memory", "node_processes", "node_logs"})
	cases := []struct {
		name    string
		call    *ToolCall
		wantErr string
	}{
		{"node_list 不需要参数", &ToolCall{Name: "node_list", Args: map[string]string{}}, ""},
		{"node_disk 正常节点", &ToolCall{Name: "node_disk", Args: map[string]string{"node": "kind-control-plane"}}, ""},
		{"节点名可以带点", &ToolCall{Name: "node_disk", Args: map[string]string{"node": "node-1.example.com"}}, ""},
		{"node_disk 缺 node", &ToolCall{Name: "node_disk", Args: map[string]string{}}, "缺少 node 参数"},
		{"节点名带 shell 元字符", &ToolCall{Name: "node_disk", Args: map[string]string{"node": "a;rm -rf /"}}, "格式非法"},
		{"节点名带斜杠", &ToolCall{Name: "node_disk", Args: map[string]string{"node": "a/b"}}, "格式非法"},
		{"node_logs lines 合法", &ToolCall{Name: "node_logs", Args: map[string]string{"node": "kind-control-plane", "lines": "20"}}, ""},
		{"node_logs lines 为 0", &ToolCall{Name: "node_logs", Args: map[string]string{"node": "kind-control-plane", "lines": "0"}}, "lines 参数"},
		{"node_logs lines 超上限", &ToolCall{Name: "node_logs", Args: map[string]string{"node": "kind-control-plane", "lines": "600"}}, "lines 参数"},
		{"node_logs lines 不是数字", &ToolCall{Name: "node_logs", Args: map[string]string{"node": "kind-control-plane", "lines": "abc"}}, "lines 参数"},
		{"没注册过的节点工具名", &ToolCall{Name: "node_foo", Args: map[string]string{"node": "kind-control-plane"}}, "未知工具"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateToolCall("demo", c.call)
			if c.wantErr == "" && err != nil {
				t.Errorf("ValidateToolCall 应该通过，得到错误: %v", err)
			}
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ValidateToolCall 应该报错（%s），得到 nil", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("错误 = %q, 应该包含 %q", err.Error(), c.wantErr)
				}
			}
		})
	}
}

func TestExecuteToolRoutesNodeToolToMCP(t *testing.T) {
	// 关键验证：模型调 node_disk 时，分发走 MCP 接口，
	// 不经过 ToolsAdapter（k8s 工具的调用次数必须还是 0）。
	tools := &stubTools{}
	fake := &fakeNodeTools{tools: map[string]bool{"node_disk": true}, output: "disk-output"}
	ag := &Agent{Tools: tools, MCP: fake}
	out, err := ag.executeTool(context.Background(), &ToolCall{ID: "c1", Name: "node_disk", Args: map[string]string{"node": "kind-control-plane"}})
	if err != nil {
		t.Fatalf("executeTool node_disk 失败: %v", err)
	}
	if out.Summary != "disk-output" {
		t.Errorf("executeTool 摘要 = %q, want %q", out.Summary, "disk-output")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("MCP 调用记录 = %v, want 1 次 node_disk", fake.calls)
	}
	if tools.calls != 0 {
		t.Errorf("k8s 工具被调了 %d 次，node_disk 不该经过 ToolsAdapter", tools.calls)
	}
	// k8s 工具仍走老路：get_pod 应该到 stubTools，不到 MCP。
	out, err = ag.executeTool(context.Background(), &ToolCall{ID: "c2", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-xxx"}})
	if err != nil {
		t.Fatalf("executeTool get_pod 失败: %v", err)
	}
	if out.Summary != "Pod ok" {
		t.Errorf("get_pod 摘要 = %q, want %q", out.Summary, "Pod ok")
	}
	if len(fake.calls) != 1 {
		t.Errorf("get_pod 不该调 MCP，调用记录 = %v", fake.calls)
	}
}

func TestExecuteToolNodeToolWithoutMCP(t *testing.T) {
	// 没有 MCP Client 时（MCP 为 nil），节点工具调不动，
	// 但 k8s 工具不受影响——降级的意思就是这个。
	RegisterNodeTools([]string{"node_disk"})
	tools := &stubTools{}
	ag := &Agent{Tools: tools}
	_, err := ag.executeTool(context.Background(), &ToolCall{ID: "c1", Name: "node_disk", Args: map[string]string{"node": "kind-control-plane"}})
	if err == nil {
		t.Fatalf("没有 MCP 时调 node_disk 应该报错")
	}
	out, err := ag.executeTool(context.Background(), &ToolCall{ID: "c2", Name: "get_pod", Args: map[string]string{"namespace": "demo", "pod": "demo-api-xxx"}})
	if err != nil {
		t.Fatalf("没有 MCP 时 get_pod 应该照常工作: %v", err)
	}
	if out.Summary != "Pod ok" {
		t.Errorf("get_pod 摘要 = %q, want %q", out.Summary, "Pod ok")
	}
}
