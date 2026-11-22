package chat

import (
	"context"
	"strings"
	"testing"

	"kubecause/internal/agent"
	"kubecause/internal/kube"
	"kubecause/internal/mcpclient"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

// fakeNodeTools 是 agent.NodeTools 的测试替身：记录调用，返回预设输出。
type fakeNodeTools struct {
	tools  map[string]bool
	output string
	err    error
	calls  []string
}

func (f *fakeNodeTools) HasTool(name string) bool { return f.tools[name] }

func (f *fakeNodeTools) CallTool(_ context.Context, name string, args map[string]string) (agent.ToolOutput, error) {
	f.calls = append(f.calls, name+" node="+args["node"])
	if f.err != nil {
		return agent.ToolOutput{}, f.err
	}
	return agent.ToolOutput{Summary: f.output, Data: map[string]any{"text": f.output}}, nil
}

func TestExecuteChatToolRoutesNodeToolToMCP(t *testing.T) {
	// 对话里模型调 node_disk 时，分发走 MCP 接口，不经过 ToolsAdapter。
	agent.RegisterNodeTools([]string{"node_list", "node_disk", "node_memory", "node_processes", "node_logs"})
	fake := &fakeNodeTools{tools: map[string]bool{"node_disk": true, "node_list": true}, output: "disk-output"}
	s := &Service{MCP: fake}
	out, err := s.executeChatTool(context.Background(), &agent.ToolCall{ID: "c1", Name: "node_disk", Args: map[string]string{"node": "kind-control-plane"}})
	if err != nil {
		t.Fatalf("executeChatTool node_disk 失败: %v", err)
	}
	if out.Summary != "disk-output" {
		t.Errorf("executeChatTool 摘要 = %q, want %q", out.Summary, "disk-output")
	}
	if len(fake.calls) != 1 || fake.calls[0] != "node_disk node=kind-control-plane" {
		t.Fatalf("MCP 调用记录 = %v, want 1 次 node_disk", fake.calls)
	}
	// node_list 也不带 namespace 参数，同样走 MCP。
	out, err = s.executeChatTool(context.Background(), &agent.ToolCall{ID: "c2", Name: "node_list", Args: map[string]string{}})
	if err != nil {
		t.Fatalf("executeChatTool node_list 失败: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("MCP 调用记录 = %v, want 2 次", fake.calls)
	}
}

func TestExecuteChatToolNodeToolWithoutMCP(t *testing.T) {
	// 没有 MCP Client 时，对话里调 node_disk 应该报错，
	// 而不是悄悄走到 k8s 分发的 default 分支之外的地方。
	agent.RegisterNodeTools([]string{"node_disk"})
	s := &Service{}
	_, err := s.executeChatTool(context.Background(), &agent.ToolCall{ID: "c1", Name: "node_disk", Args: map[string]string{"node": "kind-control-plane"}})
	if err == nil {
		t.Fatalf("没有 MCP 时调 node_disk 应该报错")
	}
	if !strings.Contains(err.Error(), "未知工具") {
		t.Errorf("错误 = %q, 应该说明工具未知", err.Error())
	}
}

// TestChatRealModelCallsNodeToolViaMCP 是整条链的端到端验证：
// 剧本模型第一步调 node_disk，第二步给最终文本；
// 断言三件事：发给模型的工具表里有 node_disk、
// 调用真的走了 MCP（fake 有记录）、回复里有最终文本。
// 这比单测分发更进一步：模型、runReal、分发、MCP 全在链上。
func TestChatRealModelCallsNodeToolViaMCP(t *testing.T) {
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下节点的磁盘。","tool_calls":[{"id":"call-node-1","type":"function","function":{"name":"node_disk","arguments":"{\"node\":\"kind-control-plane\"}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"节点磁盘还剩很多，问题不在磁盘。"}}]}`
	srv, bodies, _ := scriptedModelServer(t, []string{resp1, resp2})
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	defer s.Close()
	real := model.NewRealWithConfig(model.RealConfig{
		APIKey: "test-key", BaseURL: srv.URL, ModelName: "mock",
		HTTPClient: srv.Client(), MaxRetries: 0,
	})
	real.SetExtraTools([]mcpclient.ToolDef{{
		Name: "node_disk", Description: "查看指定节点的磁盘用量",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"node": map[string]any{"type": "string"}}, "required": []string{"node"}},
	}})
	agent.RegisterNodeTools([]string{"node_list", "node_disk", "node_memory", "node_processes", "node_logs"})
	fake := &fakeNodeTools{tools: map[string]bool{"node_disk": true}, output: "Filesystem Size Used Avail Use%\n/dev/vda1 100G 20G 80G 20%"}
	svc := NewService(s, &run.ToolsAdapter{Kube: kube.NewFake()}, real)
	svc.MCP = fake
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 那个 Pod 所在的节点磁盘怎么样")
	if err != nil {
		t.Fatalf("对话失败: %v", err)
	}
	// 1. 发给模型的工具表里必须有 node_disk：模型是看到工具才调的。
	if len(*bodies) == 0 || !strings.Contains((*bodies)[0], `"name":"node_disk"`) {
		t.Fatalf("第一次发给模型的请求里应包含 node_disk 工具，得到：%v", *bodies)
	}
	// 2. 调用真的走了 MCP：fake 有记录，且只调了 1 次。
	if len(fake.calls) != 1 || fake.calls[0] != "node_disk node=kind-control-plane" {
		t.Fatalf("MCP 调用记录 = %v, want 1 次 node_disk", fake.calls)
	}
	// 3. 回复里有模型顺带的文字和最终文本。
	if !strings.Contains(r.Reply, "我先看一下节点的磁盘。") {
		t.Fatalf("调工具时顺带的文字应拼进最终回复，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "节点磁盘还剩很多，问题不在磁盘。") {
		t.Fatalf("最终文本应在回复里，得到 %q", r.Reply)
	}
	// 4. 工具记录里 node_disk 这一行有摘要（前端时间线能看到）。
	found := false
	for _, rec := range r.ToolCalls {
		if rec.Name == "node_disk" {
			found = true
			if !strings.Contains(rec.Summary, "/dev/vda1") {
				t.Errorf("node_disk 的工具摘要 = %q, 应该包含 MCP 返回的磁盘输出", rec.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("ToolCalls 里没有 node_disk 的记录：%v", r.ToolCalls)
	}
}
