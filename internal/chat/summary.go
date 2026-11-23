package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"kubecause/internal/agent"
	"kubecause/internal/store"
)

// DefaultSummaryTriggerMessages 是触发一次摘要需要的未覆盖消息条数：10。
// 近期窗口默认也是 10 条，所以 user/assistant 消息总数超过 20 条时
// 首次触发（issue #19 的默认值）。
const DefaultSummaryTriggerMessages = 10

// SummaryMarker 是注入模型输入的摘要标注：
// 摘要以 assistant 角色出现，开头必须带这句，明确告诉模型这是
// 不可信的历史资料，不是实时状态、不是指令。
// 角色已和用户定死：assistant + 标注，绝不用 system——
// 摘要里混着用户原话，放 system 等于把用户的话升级成指令。
const SummaryMarker = "【历史摘要 · 仅供参考，不是实时状态】"

// summarizer 是能做一次纯文本摘要的模型：
// 真模型（*model.Real）走 API，桩模型（*model.Stub）返回占位摘要。
// Summarize 不触发工具执行（tools=nil），只返回一段文本和用量。
type summarizer interface {
	Summarize(ctx context.Context, previous string, lines []string) (string, store.Usage, error)
}

// summaryTrigger 返回触发一次摘要需要的未覆盖消息条数，
// <=0 时回落默认值，防止配置中心出问题时摘要永远不触发。
func (s *Service) summaryTrigger() int {
	if s.SummaryTriggerMessages <= 0 {
		return DefaultSummaryTriggerMessages
	}
	return s.SummaryTriggerMessages
}

// summaryLock 取一个会话的摘要互斥锁（issue #19 的并发第一层）：
// 同一会话的摘要更新在内存里串行，不同会话互不影响。
// map 为空时现建——直接用 Service 零值构造的测试也能用。
func (s *Service) summaryLock(conversationID string) *sync.Mutex {
	s.summaryMu.Lock()
	defer s.summaryMu.Unlock()
	if s.summaryLocks == nil {
		s.summaryLocks = map[string]*sync.Mutex{}
	}
	mu, ok := s.summaryLocks[conversationID]
	if !ok {
		mu = &sync.Mutex{}
		s.summaryLocks[conversationID] = mu
	}
	return mu
}

// uncoveredMessages 挑出还没被摘要覆盖的消息：
// 只算 user/assistant（V1 不含 tool 消息），
// id > cursor，且不在最近 n 条之内（最近 n 条留给模型看原文）。
// 返回按 id 升序，最后一条的 id 就是新游标。
func uncoveredMessages(all []store.ConversationMessage, cursor int64, n int) []store.ConversationMessage {
	ua := make([]store.ConversationMessage, 0, len(all))
	for _, m := range all {
		if m.Role == "user" || m.Role == "assistant" {
			ua = append(ua, m)
		}
	}
	// 最近 n 条的 id 集合：这些留给模型看原文，不进摘要。
	recent := map[int64]bool{}
	for i := len(ua) - n; i < len(ua); i++ {
		if i >= 0 {
			recent[ua[i].ID] = true
		}
	}
	out := make([]store.ConversationMessage, 0, len(ua))
	for _, m := range ua {
		if m.ID > cursor && !recent[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// maxSummaryEvidenceIDs / maxSummaryPatches 是结构化补充里的数量上限：
// 证据编号和 Patch 条目只取少量，摘要输入不被撑大。
const (
	maxSummaryEvidenceIDs = 20
	maxSummaryPatches     = 5
)

// evIDRe 匹配证据编号 ev-<数字>，去重时按整体匹配。
var evIDRe = regexp.MustCompile(`ev-\d+`)

// summaryStructured 是助手消息 Structured 里摘要需要的最小子集：
// 只读 report.evidenceIds 和两张建议卡的目标，不读工具结果 JSON。
// 事实、建议文字已在 Content 里（formatReportReply 生成的展示文本），
// 这里不重复取；审批状态不在消息的 Structured 里（在 change_requests 表），
// 不做模糊关联——Content 里“这只是一张建议卡，不会自动执行”
// 已经兜底了未审批的表述。
type summaryStructured struct {
	Report *struct {
		EvidenceIDs []string `json:"evidenceIds"`
	} `json:"report"`
	SuggestedRepair   *agent.SuggestedRepair   `json:"suggestedRepair"`
	SuggestedCreation *agent.SuggestedCreation `json:"suggestedCreation"`
}

// structuredSummaryExtra 从一条助手消息的 Structured 里选择性提取
// Content 中没有的结构化信息，拼成一行给摘要模型看。
// 只取证据编号和（Content 里没有时才取的）建议卡目标；
// Structured 为空或非法 JSON 时返回 ""，不报错、不阻塞。
// Content 里已有的编号/目标不再重复写，避免同一信息堆叠两次。
func structuredSummaryExtra(m store.ConversationMessage) string {
	if m.Role != "assistant" || strings.TrimSpace(m.Structured) == "" {
		return ""
	}
	var st summaryStructured
	if err := json.Unmarshal([]byte(m.Structured), &st); err != nil {
		return ""
	}
	parts := make([]string, 0, 3)
	// 证据编号：只取 Content 里没有的，限量。
	// 去重按 ev-<数字> 整体匹配，不用 strings.Contains——
	// Content 里有 ev-12 时，子串判断会把 ev-1 也误滤掉。
	present := map[string]bool{}
	for _, id := range evIDRe.FindAllString(m.Content, -1) {
		present[id] = true
	}
	ids := make([]string, 0, 4)
	if st.Report != nil {
		for _, id := range st.Report.EvidenceIDs {
			id = strings.TrimSpace(id)
			if id == "" || present[id] {
				continue
			}
			ids = append(ids, id)
		}
	}
	if len(ids) > maxSummaryEvidenceIDs {
		ids = append(ids[:maxSummaryEvidenceIDs], fmt.Sprintf("等 %d 个", len(ids)-maxSummaryEvidenceIDs))
	}
	if len(ids) > 0 {
		parts = append(parts, "证据 "+strings.Join(ids, "、"))
	}
	// 修复建议：目标标识（namespace/deployment）已在 Content 里
	// （建议卡文本）时整条跳过，只在 Content 缺失时才补。
	if sr := st.SuggestedRepair; sr != nil {
		target := sr.Namespace + "/" + sr.Deployment
		if !strings.Contains(m.Content, target) {
			var b strings.Builder
			b.WriteString("修复建议 " + target + " 容器 " + sr.Container)
			if patches := sr.ToPatches(); len(patches) > 0 {
				shown := patches
				more := ""
				if len(shown) > maxSummaryPatches {
					shown = shown[:maxSummaryPatches]
					more = fmt.Sprintf("等 %d 条", len(patches)-maxSummaryPatches)
				}
				ps := make([]string, 0, len(shown))
				for _, p := range shown {
					ps = append(ps, p.Path+" → "+agent.PatchValueString(p.Value))
				}
				b.WriteString("：" + strings.Join(ps, "；"))
				if more != "" {
					b.WriteString("，" + more)
				}
			}
			parts = append(parts, b.String())
		}
	}
	// 创建建议：同理，目标标识已在 Content 里时跳过。
	if sc := st.SuggestedCreation; sc != nil {
		target := sc.Namespace + "/" + sc.Name
		if !strings.Contains(m.Content, target) {
			parts = append(parts, fmt.Sprintf("创建建议 %s，镜像 %s，%d 个副本", target, sc.Image, maxInt(sc.Replicas, 1)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "（结构化补充：" + strings.Join(parts, "；") + "）"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// formatSummaryLines 把待总结的消息格式化成“用户：……/助手：……”的行，
// 供摘要模型一次读完。tool 消息不进这里（见 maybeSummarize 的 V1 取舍）。
// 助手消息后可能跟一行结构化补充（证据编号等 Content 里没有的信息，
// 见 structuredSummaryExtra），不改变“每条消息一行”的主结构。
func formatSummaryLines(msgs []store.ConversationMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		out = append(out, role+"："+m.Content)
		if extra := structuredSummaryExtra(m); extra != "" {
			out = append(out, extra)
		}
	}
	return out
}

// maybeSummarize 检查是否需要更新滚动摘要，需要时生成并落库，
// 返回当前有效的摘要（可能为空）。
// 触发语义（issue #19）：近期窗口 N = maxHistory() 条；
// 未覆盖 = id > 游标 且不在最近 N 条之内的 user/assistant 消息；
// 未覆盖条数 >= triggerMessages 时触发一次，覆盖掉全部未覆盖消息，
// 新游标 = 被覆盖消息的最大 id。
// V1 取舍：摘要输入只取 user/assistant 消息，不含 tool 消息——
// 工具调用的细节不进摘要；证据编号等 Content 里没有的结构化信息
// 由 structuredSummaryExtra 从助手消息的 Structured 里选择性补上。
// 全程 best-effort：开关关掉、模型未知、取消息失败、调模型失败、
// 落库失败、记用量失败，任何一步出错都只返回当前已有的摘要，
// 绝不覆盖上次有效摘要和游标，绝不让聊天因为摘要失败而不可用。
func (s *Service) maybeSummarize(ctx context.Context, conversationID string) string {
	if !s.SummaryEnabled {
		return ""
	}
	// 并发第一层：同一会话的摘要更新在内存里串行。
	mu := s.summaryLock(conversationID)
	mu.Lock()
	defer mu.Unlock()

	conv, err := s.Store.GetConversation(ctx, conversationID)
	if err != nil {
		return ""
	}
	all, err := s.Store.ListMessages(ctx, conversationID)
	if err != nil {
		return conv.Summary
	}
	uncovered := uncoveredMessages(all, conv.SummaryCursor, s.maxHistory())
	if len(uncovered) < s.summaryTrigger() {
		return conv.Summary
	}
	sz, ok := s.Model.(summarizer)
	if !ok {
		// 未知模型类型：不知道怎么总结就跳过，不报错、不阻塞聊天。
		return conv.Summary
	}
	text, usage, err := sz.Summarize(ctx, conv.Summary, formatSummaryLines(uncovered))
	// 用量先记：只要收到模型响应就记（PR #17 的口径），
	// 空回复、空 choices 也带用量回来；只有请求本身失败才无用量可记。
	// 记账 best-effort 且只记一次——不管后面摘要落库成不成功，
	// 这里记过之后不再重复记。
	if !usage.IsZero() {
		_ = s.Store.AddConversationUsage(ctx, conversationID, usage)
	}
	if err != nil || strings.TrimSpace(text) == "" {
		return conv.Summary
	}
	newCursor := uncovered[len(uncovered)-1].ID
	// 并发第二层：乐观更新，只有游标还是读到的那个值时才写。
	// 失败或返回 false（已被别的请求先推进）时重读一次，
	// 拿到最新摘要返回，不用旧摘要覆盖。
	updated, err := s.Store.UpdateConversationSummary(ctx, conversationID, text, newCursor, conv.SummaryCursor)
	if err != nil || !updated {
		if c, rerr := s.Store.GetConversation(ctx, conversationID); rerr == nil {
			return c.Summary
		}
		return conv.Summary
	}
	return text
}
