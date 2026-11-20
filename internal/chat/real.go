package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/model"
	"kubecause/internal/store"
)

// chatSystemPrompt 是对话模式的系统提示。
// 关键有三点，写在这里而不是藏在代码逻辑里：
//  1. 指代消解：用户说的“这个 ns”、“刚才那个 Pod”、“它”
//     要按会话历史里的最近提到的目标来理解，理解不了就主动问，
//     不要猜一个 namespace 去查；
//  2. 只读：只能调只读工具，不能执行任何修改，
//     需要修复时只在回答里给出修复建议，由用户转正式诊断、人工审批；
//  3. 诚实：证据不足就说证据不足，不要编结论、编数字。
const chatSystemPrompt = `你是 Kubernetes 故障诊断助手，正在和用户多轮对话。

规则：
1. 只能调用提供的只读工具，不能执行任何修改。需要修复时，只在回答里给出修复建议，不要自己动手改集群。
2. 先用发现工具（get_cluster_overview、list_problem_pods、list_namespaces）看全貌、找问题，再用定向工具（get_pod、get_events、get_pod_logs、get_deployment、search_runbooks）深挖。一次只调一个工具。
3. 指代消解：用户说的“这个命名空间”、“刚才那个 Pod”、“它”、“那个一直重启的”要按会话历史里的最近提到的目标来理解。如果历史里没有明确的目标，或者有多个可能的目标，不要猜，直接问用户是哪个命名空间、哪个 Pod。
4. 系统命名空间可以查、可以诊断，但不要把它当成用户业务下结论：如果查到的有问题 Pod 在 kube-system 等系统命名空间里，要明确说明这是系统组件，并且不要给它出修复建议——修复校验本来就会拒绝系统命名空间的修改。
5. 证据不足时不要硬编结论，要说还需要补查什么。回答用中文，简明扼要，先给结论再给依据。
6. 如果证据明确指向容器内存 limit 过低，可以在回答最后给出修复建议（目标、旧值、新值、理由），并提醒用户：这只是建议，要创建可审批的修复任务、人工审批后才会执行。
7. 修复建议必须通过 submit_diagnosis 的 suggestedRepair 结构化字段返回，不要只在自由文本里提一句。用户如果明确说了增加量（例如“增加 128Mi”、“调到 256Mi”），按用户说的算新值：先查当前值，再按用户说的增量或目标值算，不要默认翻倍。没有用户明确要求时，也按证据算一个合理的新值，不要只会翻倍。
8. 修复决策只能由你做：后端只做校验，不会替你合成建议。如果你给不出结构化建议，就老实说证据不足、还需要补查什么，不要编一个建议。`

// runReal 是真模型的对话循环：
// 每轮最多调 MaxChatSteps 次工具，每次问模型一步，
// 模型调工具就执行并把结果以 tool 角色追加进历史，
// 模型给最终文本或提交诊断报告就结束本轮。
// 历史只带最近 MaxHistoryMessages 条，更早的截断（见 chat.go）。
func (s *Service) runReal(ctx context.Context, conversationID, text string, history []store.ConversationMessage, m *model.Real) (*Reply, error) {
	msgs := []model.ChatMessage{{Role: "system", Content: chatSystemPrompt}}
	for _, h := range history {
		switch h.Role {
		case "user":
			msgs = append(msgs, model.ChatMessage{Role: "user", Content: h.Content})
		case "assistant":
			msgs = append(msgs, model.ChatMessage{Role: "assistant", Content: h.Content})
		case "tool":
			// 历史里的 tool 消息在上一轮已是独立行，
			// 给模型时折成一条 user 消息，避免 tool_call_id 对不上
			// 上一轮的 assistant 消息导致协议报错。
			msgs = append(msgs, model.ChatMessage{Role: "user", Content: "工具返回：" + h.Content})
		}
	}
	msgs = append(msgs, model.ChatMessage{Role: "user", Content: text})

	var recs []ToolCallRecord
	var outputs []agent.ToolOutput
	structured := map[string]any{}
	lastNS := s.resolveNamespace(ctx, text, history)
	// askedForStructured 标记是否已经向模型追问过一次结构化建议。
	// 真模型路径的修复决策只走模型单一路：模型没给结构化建议时，
	// 不调 synthesizeRepair 去合成一张卡（那是桩模型测试用的规则，
	// 真模型跑时不用）。做法是再问模型一次，让它把 suggestedRepair
	// 补回来；补不回来就老实说还没法下结论，不要编一张卡。
	askedForStructured := false

	for step := 0; step < MaxChatSteps; step++ {
		st, err := m.Chat(ctx, msgs)
		if err != nil {
			return nil, fmt.Errorf("调用模型失败: %w", err)
		}
		if st.IsFinal {
			replyText := st.Content
			var sr *agent.SuggestedRepair
			if st.Report != nil {
				// 报告刚拿到就归一化：缺失的切片字段补成空数组，
				// 否则落盘再序列化成 null，前端读 .length 会白屏。
				st.Report.Normalize()
				replyText = formatReportReply(st.Report)
				structured["report"] = st.Report
				sr = st.Report.SuggestedRepair
			}
			if sr == nil && !askedForStructured {
				// 模型给了最终回答但没有结构化修复建议：追问一次，
				// 让它用 submit_diagnosis 把建议补回来，而不是由后端合成。
				askedForStructured = true
				msgs = append(msgs, model.ChatMessage{Role: "user", Content: "如果你认为需要修复，请调用 submit_diagnosis 提交结构化诊断报告，并在 suggestedRepair 里给出目标、旧值、新值和理由（用户如果明确说了增加量，按用户说的算）。如果证据不足以给修复建议，请直接说明还需要补查什么，不要编一个建议。"})
				continue
			}
			if sr != nil {
				structured["suggestedRepair"] = sr
				replyText += fmt.Sprintf("\n修复建议：%s/%s 容器 %s 的内存 limit 从 %s 调到 %s（%s）。这只是一张建议卡，不会自动执行——想走正式流程可以点“创建可审批的修复任务”，生成提案后人工审批再执行。",
					sr.Namespace, sr.Deployment, sr.Container, sr.OldValue, sr.NewValue, sr.Reason)
			}
			return &Reply{Reply: replyText, ToolCalls: recs, Structured: structured, SuggestedRepair: sr}, nil
		}
		if st.ToolCall == nil {
			return nil, fmt.Errorf("模型既没有调工具也没有返回文本")
		}
		tc := st.ToolCall
		// 模型漏填 namespace 时，用上文解析到的 namespace 补齐；
		// 补不齐时不猜，让工具校验报错，模型下一轮会主动问用户。
		if tc.Args == nil {
			tc.Args = map[string]string{}
		}
		if _, ok := tc.Args["namespace"]; !ok || tc.Args["namespace"] == "" {
			if !isDiscovery(tc.Name) && tc.Name != "search_runbooks" && lastNS != "" {
				tc.Args["namespace"] = lastNS
			}
		}
		if ns := tc.Args["namespace"]; ns != "" {
			lastNS = ns
		}
		start := time.Now()
		out, err := s.executeChatTool(ctx, tc)
		rec := ToolCallRecord{ID: tc.ID, Name: tc.Name, Args: tc.Args, DurationMs: time.Since(start).Milliseconds()}
		if err != nil {
			rec.Error = err.Error()
			rec.Summary = "工具执行失败：" + err.Error()
			recs = append(recs, rec)
			s.recordTool(ctx, conversationID, rec, agent.ToolOutput{Summary: rec.Summary})
			// 工具失败回传给模型：下一轮它能看到失败原因，
			// 换个工具重试，而不是原地重复调同一个。
			msgs = append(msgs,
				model.ChatMessage{Role: "assistant", Content: "", ToolCalls: []model.ChatToolCall{{ID: tc.ID, Name: tc.Name, Arguments: mustJSON(tc.Args)}}},
				model.ChatMessage{Role: "tool", Content: "工具执行失败：" + err.Error(), ToolCallID: tc.ID},
			)
			continue
		}
		rec.Summary = out.Summary
		recs = append(recs, rec)
		outputs = append(outputs, out)
		structured[tc.Name] = out.Data
		// 规范 key 映射（Slice 7 修的“后续追问只剩纯文本”）：
		// 以前这里只存了 structured[工具名]，例如 structured["get_pod"]，
		// 但前端 buildBlocks 只认规范 key（overview、problemPods、pod……），
		// 工具名当 key 的结果会被过滤掉，结果就是聊天里只剩文字、没有卡片，
		// 刷新后也一样。现在每个工具的结果同时写一份规范 key，
		// 工具名那份留着做追溯，两份都落盘，刷新后卡片还在。
		switch tc.Name {
		case "get_cluster_overview":
			structured["overview"] = out.Data
		case "list_problem_pods":
			structured["problemPods"] = out.Data
		case "list_workload_pods":
			structured["pods"] = out.Data
		case "get_pod":
			structured["pod"] = out.Data
		case "get_deployment":
			structured["deployment"] = out.Data
		case "list_deployments":
			structured["deployments"] = out.Data
		case "get_events":
			structured["events"] = out.Data
		case "get_pod_logs":
			structured["logs"] = out.Data
		case "search_runbooks":
			structured["runbooks"] = out.Data
		}
		s.recordTool(ctx, conversationID, rec, out)
		msgs = append(msgs,
			model.ChatMessage{Role: "assistant", Content: "", ToolCalls: []model.ChatToolCall{{ID: tc.ID, Name: tc.Name, Arguments: mustJSON(tc.Args)}}},
			model.ChatMessage{Role: "tool", Content: out.Summary + "\n" + mustJSON(out.Data), ToolCallID: tc.ID},
		)
	}
	return nil, fmt.Errorf("对话已达最大工具步数（%d 步），请换个更具体的问题再问", MaxChatSteps)
}

// formatReportReply 把模型提交的结构化诊断报告转成给用户看的文本。
func formatReportReply(rep *agent.Report) string {
	var b strings.Builder
	b.WriteString("诊断结论：" + rep.Cause + "。")
	if len(rep.Facts) > 0 {
		b.WriteString("\n事实依据：\n")
		for _, f := range rep.Facts {
			b.WriteString("- " + f + "\n")
		}
	}
	if len(rep.Recommendations) > 0 {
		b.WriteString("建议：\n")
		for _, r := range rep.Recommendations {
			b.WriteString("- " + r + "\n")
		}
	}
	if len(rep.OpenQuestions) > 0 {
		b.WriteString("还没确认：\n")
		for _, q := range rep.OpenQuestions {
			b.WriteString("- " + q + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
