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
2. 先给总览，等追问再深挖：用户问“当前状态怎么样”这种总览问题时，
   先查全集群状态，给出摘要就停下来，不要在一轮里把所有命名空间、
   所有 Pod 都查完。等用户追问某个命名空间或 Pod 时再用定向工具
   （get_pod、get_events、get_pod_logs、get_deployment、search_runbooks）深挖。
   总览调几个工具由你自己判断，不用每次都把发现工具全调一遍。
   一次只调一个工具。
3. 指代消解：用户说的“这个命名空间”、“刚才那个 Pod”、“它”、“那个一直重启的”要按会话历史里的最近提到的目标来理解。如果历史里没有明确的目标，或者有多个可能的目标，不要猜，直接问用户是哪个命名空间、哪个 Pod。
4. 系统命名空间可以查、可以诊断，但不要把它当成用户业务下结论：如果查到的有问题 Pod 在 kube-system 等系统命名空间里，要明确说明这是系统组件，并且不要给它出修复建议——修复校验本来就会拒绝系统命名空间的修改。
5. 证据不足时不要硬编结论，要说还需要补查什么。回答用中文，简明扼要，先给结论再给依据。
6. 如果证据明确指向容器内存 limit 过低，可以在回答最后给出修复建议（目标、旧值、新值、理由），并提醒用户：这只是建议，要创建可审批的修复任务、人工审批后才会执行。
7. 修复建议必须通过 submit_diagnosis 的 suggestedRepair 结构化字段返回，不要只在自由文本里提一句。用户如果明确说了增加量（例如“增加 128Mi”、“调到 256Mi”），按用户说的算新值：先查当前值，再按用户说的增量或目标值算，不要默认翻倍。没有用户明确要求时，也按证据算一个合理的新值，不要只会翻倍。
8. 修复决策只能由你做：后端只做校验，不会替你合成建议。如果你给不出结构化建议，就老实说证据不足、还需要补查什么，不要编一个建议。`

// runReal 是真模型的对话循环：
// 每轮最多调 s.maxSteps() 次工具（默认 12，配置中心可改），每次问模型一步，
// 模型调工具就执行并把结果以 tool 角色追加进历史，
// 模型给最终文本或提交诊断报告就结束本轮。
// 历史只带最近 s.maxHistory() 条，更早的截断（见 chat.go）。
//
// 步数用完时不直接报错：已经查到的工具结果会整理成半成品回复返回，
// 文字里写明“步数用完了，还没来得及总结”，再问用户想深入查哪个目标。
// MaxChatSteps 是预算不是决策——它只决定一轮最多查多少，
// 不决定模型该查什么；查到的东西不能因为预算用完就丢掉，
// 否则用户看到的是一片空白加一句报错，等于白查了。
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

	for step := 0; step < s.maxSteps(); step++ {
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
	// 步数用完了：不直接 return error。
	// 已经查到的工具结果（outputs 的摘要 + structured 的规范 key）
	// 整理成半成品回复返回，RunMessage 会把它正常落盘成一条助手消息，
	// 前端按 structured 里的 overview / problemPods 正常渲染卡片和表格，
	// 用户看到的是已经查到的东西，而不是空白加报错。
	return buildPartialReply(recs, outputs, structured), nil
}

// buildPartialReply 把步数用完时已查到的工具结果整理成半成品回复。
// 文字里诚实说明两件事：已经查到了什么、还没来得及做什么（总结），
// 再问用户想深入查哪个命名空间或 Pod，让下一轮从那里继续，
// 而不是让用户换个更具体的问题重问——问题本身没错，
// 错的是后端把“预算用完”当成“用户问错了”报出来。
func buildPartialReply(recs []ToolCallRecord, outputs []agent.ToolOutput, structured map[string]any) *Reply {
	var b strings.Builder
	b.WriteString("已经查到了这些，但步数用完了，还没来得及总结：\n")
	seen := map[string]bool{}
	for _, out := range outputs {
		if out.Summary == "" || seen[out.Summary] {
			continue
		}
		seen[out.Summary] = true
		b.WriteString("- " + out.Summary + "\n")
	}
	if len(outputs) == 0 {
		b.WriteString("- 这一轮还没查到任何结果。\n")
	}
	b.WriteString("你想深入查哪个命名空间或 Pod？告诉我名字，我接着查。")
	return &Reply{Reply: b.String(), ToolCalls: recs, Structured: structured}
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
