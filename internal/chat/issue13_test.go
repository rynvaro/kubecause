package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

// chatRequestMessage 是 chat completions 请求体里的一条消息（测试用）。
type chatRequestMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content,omitempty"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

func chatRequestMessages(t *testing.T, body string) []chatRequestMessage {
	t.Helper()
	var req struct {
		Messages []chatRequestMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("请求体解析失败: %v", err)
	}
	return req.Messages
}

func countUserMessages(msgs []chatRequestMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

func TestChatParseErrorOnLastRoundDegradesDirectly(t *testing.T) {
	// 最后一轮才报解析错误：没有下一轮可跑纠错，不安排纠错，
	// 直接走带格式错误说明的降级，不要掉进通用的“步数用完”提示。
	// maxSteps=1，这一轮本身就是最后一轮。
	ctx := context.Background()
	bad := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{bad})
	svc, _ := newRealService(t, srv)
	svc.MaxSteps = 1
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么一直重启")
	if err != nil {
		t.Fatalf("最后一轮解析错误应降级为部分回复，不应报错: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("最后一轮不应发纠错请求，应只发 1 次，得到 %d 次", got)
	}
	if !strings.Contains(r.Reply, "报告生成失败") || !strings.Contains(r.Reply, "facts 应为字符串数组，收到 number") {
		t.Fatalf("降级回复应说明是报告格式错误，得到 %q", r.Reply)
	}
	if strings.Contains(r.Reply, "步数用完了") {
		t.Fatalf("最后一轮解析错误不应报“步数用完了”，得到 %q", r.Reply)
	}
}

func TestChatParseErrorOnLastRoundAfterToolSuccess(t *testing.T) {
	// maxSteps=2：第一轮成功查了总览，第二轮（最后一轮）才报解析错误。
	// 纠错没机会执行，直接降级；前面查到的总览卡片还在。
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下总览。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	svc.MaxSteps = 2
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("最后一轮解析错误应降级为部分回复，不应报错: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("最后一轮不应发纠错请求，应只发 2 次，得到 %d 次", got)
	}
	if !strings.Contains(r.Reply, "报告生成失败") {
		t.Fatalf("降级回复应说明是报告格式错误，得到 %q", r.Reply)
	}
	if strings.Contains(r.Reply, "步数用完了") {
		t.Fatalf("最后一轮解析错误不应报“步数用完了”，得到 %q", r.Reply)
	}
	if r.Structured["overview"] == nil {
		t.Fatalf("降级回复的 Structured 里应保留总览卡片")
	}
}

func TestChatStringFactsDiagnosisReturnsReport(t *testing.T) {
	// issue #13 的截图复现（对话层）：facts 是字符串而不是数组，
	// 解析放宽后一次就过，不再报“调用模型失败”。
	ctx := context.Background()
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":\"节点状态正常\",\"cause\":\"节点状态正常，Pod 未受节点影响\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "帮我诊断一下这个节点")
	if err != nil {
		t.Fatalf("facts 为字符串时对话不应失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("兼容值不应触发纠错，应只发 1 次请求，得到 %d 次", got)
	}
	if r.Structured["report"] == nil {
		t.Fatalf("Structured 里应有 report")
	}
	if !strings.Contains(r.Reply, "节点状态正常") {
		t.Fatalf("回复里应有事实依据，得到 %q", r.Reply)
	}
}

func TestChatMalformedDiagnosisCorrectsAndSucceeds(t *testing.T) {
	// 第一次交报告时 facts 是数字（完全不兼容），纠错一次后模型重交正确报告。
	ctx := context.Background()
	bad := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	good := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"demo-api 重启 5 次\"],\"cause\":\"容器内存限制过低\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, bodies, calls := scriptedModelServer(t, []string{bad, good})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么一直重启")
	if err != nil {
		t.Fatalf("纠错后对话不应失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("纠错应只多发 1 次请求，总共 2 次，得到 %d 次", got)
	}
	if r.Structured["report"] == nil {
		t.Fatalf("重交后 Structured 里应有 report")
	}
	if !strings.Contains(r.Reply, "诊断结论：容器内存限制过低") {
		t.Fatalf("重交后回复里应有格式化报告，得到 %q", r.Reply)
	}
	// 第 2 次请求的末尾正好是一对 assistant+tool 纠错消息，没有假 user 消息。
	msgs := chatRequestMessages(t, (*bodies)[1])
	if len(msgs) < 2 {
		t.Fatalf("第 2 次请求应带纠错消息，得到 %+v", msgs)
	}
	assistantMsg, toolMsg := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if assistantMsg.Role != "assistant" || len(assistantMsg.ToolCalls) != 1 || assistantMsg.ToolCalls[0].Function.Name != "submit_diagnosis" {
		t.Fatalf("纠错应追加 assistant + 失败的 submit_diagnosis，得到 %+v", assistantMsg)
	}
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call-1" || !strings.Contains(toolMsg.Content, "facts 应为字符串数组，收到 number") {
		t.Fatalf("纠错的 tool 消息应带字段错误，得到 %+v", toolMsg)
	}
	if n := countUserMessages(msgs); n != 1 {
		t.Fatalf("纠错不应追加 user 消息，user 消息应只有最初的 1 条，得到 %d 条", n)
	}
}

func TestChatMalformedDiagnosisCorrectsOnceThenDegrades(t *testing.T) {
	// 最完整的降级场景：先成功查了一个工具，再两次交坏报告。
	// 纠错只发生一次（总共 3 次模型请求），前面已成功的工具不重新执行，
	// 最终降级为明确标明“部分结果/报告生成失败”的回复，工具卡片还在，
	// 助手消息和错误状态正常持久化，不包装成高置信诊断。
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"我先看一下总览。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_cluster_overview","arguments":"{}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	resp3 := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-3","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":{\"a\":\"b\"},\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, bodies, calls := scriptedModelServer(t, []string{resp1, resp2, resp3})
	svc, s := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "当前状态怎么样")
	if err != nil {
		t.Fatalf("纠错失败后应降级为部分回复，不应报错: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Fatalf("纠错只发生一次，总共 3 次模型请求，得到 %d 次", got)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_cluster_overview" {
		t.Fatalf("前面已成功的工具不应被重新执行，应只有 1 次总览查询，得到 %+v", r.ToolCalls)
	}
	if !strings.Contains(r.Reply, "报告生成失败") || !strings.Contains(r.Reply, "部分结果") {
		t.Fatalf("降级回复应明确标明报告生成失败和部分结果，得到 %q", r.Reply)
	}
	if strings.Contains(r.Reply, "诊断结论：") {
		t.Fatalf("降级回复不应包装成诊断结论，得到 %q", r.Reply)
	}
	if r.Structured["overview"] == nil {
		t.Fatalf("降级回复的 Structured 里应保留总览卡片")
	}
	if r.Structured["report"] != nil {
		t.Fatalf("降级回复不应带 report")
	}
	if r.SuggestedRepair != nil || r.SuggestedCreation != nil {
		t.Fatalf("降级回复不应带任何建议卡")
	}
	// 持久化：助手消息里有错误提示，Structured 里有工具卡片。
	msgs, _ := s.ListMessages(ctx, conv)
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "报告生成失败") {
			found = true
			if !strings.Contains(m.Structured, "overview") {
				t.Fatalf("持久化的助手消息里应有总览卡片，得到 %s", m.Structured)
			}
		}
	}
	if !found {
		t.Fatalf("持久化的助手消息里应有报告生成失败的提示")
	}
	// 第 3 次请求（纠错那次）的末尾是 assistant+tool 纠错消息。
	correction := chatRequestMessages(t, (*bodies)[2])
	if len(correction) < 2 {
		t.Fatalf("第 3 次请求应带纠错消息，得到 %+v", correction)
	}
	assistantMsg, toolMsg := correction[len(correction)-2], correction[len(correction)-1]
	if assistantMsg.Role != "assistant" || toolMsg.Role != "tool" || toolMsg.ToolCallID != "call-2" {
		t.Fatalf("纠错消息应是一对 assistant+tool，得到 %+v / %+v", assistantMsg, toolMsg)
	}
	if n := countUserMessages(correction); n != 1 {
		t.Fatalf("纠错不应追加 user 消息，user 消息应只有最初的 1 条，得到 %d 条", n)
	}
}

func TestChatDegradedReplyKeepsUnverifiedAnalysis(t *testing.T) {
	// 两次都交坏报告且都附带文字：降级回复里保留最后一轮的文字，
	// 并标明未经结构化验证；不丢进模型上下文就完事，要进用户可见回复。
	ctx := context.Background()
	resp1 := `{"choices":[{"message":{"role":"assistant","content":"节点看起来没什么问题，内存还够。","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":123,\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	resp2 := `{"choices":[{"message":{"role":"assistant","content":"从节点层面看，内存和磁盘都没有压力。","tool_calls":[{"id":"call-2","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":{\"a\":\"b\"},\"cause\":\"c\",\"evidenceIds\":[],\"confidence\":\"low\"}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp1, resp2})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "帮我诊断一下这个节点")
	if err != nil {
		t.Fatalf("纠错失败后应降级为部分回复，不应报错: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("纠错只发生一次，总共 2 次模型请求，得到 %d 次", got)
	}
	if !strings.Contains(r.Reply, "报告生成失败") {
		t.Fatalf("降级回复应说明报告生成失败，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "从节点层面看，内存和磁盘都没有压力。") || !strings.Contains(r.Reply, "未经结构化验证") {
		t.Fatalf("降级回复应保留最后一轮附带的文字并标注未经结构化验证，得到 %q", r.Reply)
	}
	// 附带的文字应在部分结果之前，和“报告生成失败”的说明在同一段回复里。
	if idxText, idxPartial := strings.Index(r.Reply, "从节点层面看"), strings.Index(r.Reply, "部分结果"); idxText < 0 || idxPartial < 0 || idxText > idxPartial {
		t.Fatalf("附带的文字应放在部分结果之前，得到 %q", r.Reply)
	}
}

func TestChatInvalidRepairSuggestionDropped(t *testing.T) {
	// 修复建议本身格式非法时：报告照常返回，建议被丢弃，不产生审批卡片，
	// 丢弃的原因在回复里说清楚。
	ctx := context.Background()
	resp := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"submit_diagnosis","arguments":"{\"facts\":[\"镜像拉取失败\"],\"cause\":\"镜像 tag 不存在\",\"evidenceIds\":[],\"confidence\":\"high\",\"suggestedRepair\":\"not-an-object\"}"}}]}}]}`
	srv, _, calls := scriptedModelServer(t, []string{resp})
	svc, _ := newRealService(t, srv)
	conv := newConversation(t, svc)
	r, err := svc.RunMessage(ctx, conv, "demo-api 为什么起不来")
	if err != nil {
		t.Fatalf("修复建议非法时对话本身不应失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("应只发 1 次请求，得到 %d 次", got)
	}
	if r.SuggestedRepair != nil {
		t.Fatalf("非法的修复建议应被丢弃，得到 %+v", r.SuggestedRepair)
	}
	if _, ok := r.Structured["suggestedRepair"]; ok {
		t.Fatalf("Structured 里不应出现丢弃的修复建议卡")
	}
	if strings.Contains(r.Reply, "修复建议：") {
		t.Fatalf("不应出现修复建议卡文本，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "修复建议格式无效") {
		t.Fatalf("回复里应说明修复建议因格式无效被丢弃，得到 %q", r.Reply)
	}
	if !strings.Contains(r.Reply, "诊断结论：镜像 tag 不存在") {
		t.Fatalf("报告其余部分应照常返回，得到 %q", r.Reply)
	}
}
