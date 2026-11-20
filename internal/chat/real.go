package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
4. 工具参数里的 namespace 不要查 kube-system 等系统命名空间；不知道 namespace 时先问用户，或者先用 list_problem_pods 查全部。
5. 证据不足时不要硬编结论，要说还需要补查什么。回答用中文，简明扼要，先给结论再给依据。
6. 如果证据明确指向容器内存 limit 过低，可以在回答最后给出修复建议（目标、旧值、新值、理由），并提醒用户：这只是建议，要走正式诊断、人工审批后才会执行。`

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

	for step := 0; step < MaxChatSteps; step++ {
		st, err := m.Chat(ctx, msgs)
		if err != nil {
			return nil, fmt.Errorf("调用模型失败: %w", err)
		}
		if st.IsFinal {
			replyText := st.Content
			var sr *agent.SuggestedRepair
			if st.Report != nil {
				replyText = formatReportReply(st.Report)
				structured["report"] = st.Report
				sr = st.Report.SuggestedRepair
			}
			if sr == nil {
				// 模型没直接给建议卡时，按证据合成一张：
				// 只有 OOM 证据明确且能读到当前 limit 时才给。
				ns, dep := lastTargetFromOutputs(outputs, lastNS)
				if dep != "" {
					sr = synthesizeRepair(ctx, s, outputs, ns, dep)
				}
			}
			if sr != nil {
				structured["suggestedRepair"] = sr
				replyText += fmt.Sprintf("\n修复建议：%s/%s 容器 %s 的内存 limit 从 %s 调到 %s（%s）。这只是一张建议卡，不会自动执行——想走正式流程可以转成正式诊断，生成提案后人工审批再执行。",
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
		out, err := s.executeChatTool(ctx, tc)
		rec := ToolCallRecord{ID: tc.ID, Name: tc.Name, Args: tc.Args}
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

// lastTargetFromOutputs 从已收集的工具结果里找最近的目标，
// 供修复建议卡合成时用。
func lastTargetFromOutputs(outputs []agent.ToolOutput, fallbackNS string) (string, string) {
	for i := len(outputs) - 1; i >= 0; i-- {
		b, _ := json.Marshal(outputs[i].Data)
		var v struct {
			Deployment string `json:"deployment"`
			Namespace  string `json:"namespace"`
		}
		if err := json.Unmarshal(b, &v); err == nil && v.Deployment != "" {
			ns := v.Namespace
			if ns == "" {
				ns = fallbackNS
			}
			return ns, v.Deployment
		}
	}
	return fallbackNS, ""
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
