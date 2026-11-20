// KubeCause 前端主界面（Slice 6）。
//
// 布局：左侧会话列表，中间聊天 / 单次诊断，右侧“Agent 活动”面板。
// 两种模式共用同一套后端：对话模式是一问一答（只读），
// 单次诊断是填表单、等报告、可审批、可执行、可验证。
//
// Slice 6 的布局原则：
//   - 详情结果属于结果，回聊天区：回答文字下面直接跟按类型渲染的结果块
//     （总览卡片、Pod 表格、事件、诊断报告、修复建议卡、审批卡），
//     默认折叠一部分，不把整张表一次性顶上去；
//   - 右侧不再放详情，只放工具执行过程（活动面板），每一步一张卡片，
//     带耗时；聊天里只留一行工具摘要，点开看完整时间线；
//   - 修复建议卡上的“转成正式诊断”按钮只要有建议卡就一直显示，
//     翻历史、刷新后还在；点了之后审批卡直接出现在聊天里，
//     不绕过审批，聊天里永远不给直接“执行”按钮。
//
// 实时显示用轮询，不用 WebSocket：
//   - 对话：POST /messages 在服务端跑整轮，期间 tool 消息会陆续落盘，
//     前端每 800ms 轮询一次会话，就能实时看到“正在执行哪个工具”；
//   - 单次诊断：每 900ms 轮询 /api/runs/{id}/events?after=N 追加时间线，
//     同时刷新任务快照看状态、报告和提案。
// 滚动控制：只有用户已经在最底下时才跟着滚到底，否则显示“有新消息”提示，
// 不把正在往上翻历史的用户拽下来。

import { useCallback, useEffect, useRef, useState } from 'react'
import {
  convertConversation,
  createConversation,
  createRun,
  decideApproval,
  getConversation,
  getRun,
  listConversations,
  listRunEvents,
  postChatMessage,
} from './api'
import {
  ActivityPanel,
  ApprovalCard,
  RepairStatusCard,
  ReportView,
  RunTimeline,
  StructuredView,
  ToolSummary,
} from './components'
import type { ActivityItem } from './components'
import { parseStructured, parseToolCalls } from './types'
import type { Conversation, ConversationMessage, RunSnapshot, RunStep } from './types'

type Mode = 'chat' | 'run'

const QUICK_QUESTIONS = ['当前状态怎么样', 'demo 那个一直重启的是怎么回事', 'kube-system 现在怎么样']

function runStillActive(snap: RunSnapshot | null): boolean {
  if (!snap) return false
  if (snap.status === 'queued' || snap.status === 'running') return true
  const p = snap.proposal
  if (p && (p.executionStatus === 'executing' || p.verificationStatus === 'verifying')) return true
  return false
}

/** 从会话消息里汇总本轮调过的工具：tool 消息和助手消息里都存了同一份调用，
 * 按“消息 id + 调用 id”去重——桩模型每轮的调用 id 会从 1 重新编号，
 * 只按调用 id 去重会把第二轮的调用吞掉。 */
function chatActivityItems(msgs: ConversationMessage[]): ActivityItem[] {
  const byKey = new Map<string, ActivityItem>()
  for (const m of msgs) {
    for (const c of parseToolCalls(m)) {
      const key = `${m.id}:${c.id || c.name}`
      if (!byKey.has(key)) {
        byKey.set(key, {
          id: key,
          name: c.name,
          args: c.args,
          summary: c.summary,
          error: c.error,
          durationMs: c.durationMs ?? 0,
        })
      }
    }
  }
  return [...byKey.values()]
}

/** 单次诊断的步骤转成活动面板的卡片：报告和修复是里程碑，也各占一张卡。 */
function runActivityItems(steps: RunStep[]): ActivityItem[] {
  return steps.map((s) => {
    let name = s.toolName || s.kind
    if (s.kind === 'report') name = '诊断报告'
    if (s.kind === 'repair') name = '修复'
    if (s.kind === 'error') name = '出错'
    let args: Record<string, string> | undefined
    try {
      if (s.args) {
        const v = JSON.parse(s.args) as unknown
        if (v && typeof v === 'object' && !Array.isArray(v)) args = v as Record<string, string>
      }
    } catch {
      // args 不是 JSON 就不解析，摘要里已有文字。
    }
    return {
      id: `step-${s.seq}`,
      name,
      args,
      summary: s.error ? `失败：${s.error}` : s.summary || '',
      error: s.error,
      durationMs: s.durationMs,
    }
  })
}

export default function App() {
  const [mode, setMode] = useState<Mode>('chat')
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [activeConvId, setActiveConvId] = useState<string>('')
  const [messages, setMessages] = useState<ConversationMessage[]>([])
  const [chatInput, setChatInput] = useState('')
  const [chatBusy, setChatBusy] = useState(false)
  const [converting, setConverting] = useState(false)
  const [chatError, setChatError] = useState('')
  const [notice, setNotice] = useState('')

  const [ns, setNs] = useState('demo')
  const [kind, setKind] = useState('Deployment')
  const [name, setName] = useState('demo-api')
  const [question, setQuestion] = useState('demo-api 为什么一直重启')
  const [runId, setRunId] = useState('')
  const [snapshot, setSnapshot] = useState<RunSnapshot | null>(null)
  const [events, setEvents] = useState<RunStep[]>([])
  const [runBusy, setRunBusy] = useState(false)
  const [deciding, setDeciding] = useState(false)
  const [runError, setRunError] = useState('')
  const [runTimelineOpen, setRunTimelineOpen] = useState(false)
  const [hasNewBelow, setHasNewBelow] = useState(false)

  const afterRef = useRef(0)
  const chatPollRef = useRef<number | null>(null)
  const runPollRef = useRef<number | null>(null)
  const messagesWrapRef = useRef<HTMLDivElement>(null)
  const nearBottomRef = useRef(true)

  const refreshConversations = useCallback(async () => {
    try {
      setConversations(await listConversations())
    } catch {
      // 会话列表读不到不阻塞主流程，聊天框仍可用。
    }
  }, [])

  useEffect(() => {
    void refreshConversations()
  }, [refreshConversations])

  useEffect(() => {
    return () => {
      if (chatPollRef.current) window.clearInterval(chatPollRef.current)
      if (runPollRef.current) window.clearInterval(runPollRef.current)
    }
  }, [])

  // 滚动控制：只有已经在最底下时才跟着滚到底；用户往上翻历史时，
  // 新消息来了只显示“有新消息”按钮，不把人拽下来。
  function onMessagesScroll() {
    const el = messagesWrapRef.current
    if (!el) return
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 80
    nearBottomRef.current = near
    if (near) setHasNewBelow(false)
  }

  useEffect(() => {
    if (mode !== 'chat') return
    const el = messagesWrapRef.current
    if (!el) return
    if (nearBottomRef.current) {
      el.scrollTop = el.scrollHeight
      setHasNewBelow(false)
    } else {
      setHasNewBelow(true)
    }
  }, [messages, chatBusy, snapshot, mode])

  function scrollToBottom() {
    const el = messagesWrapRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
    nearBottomRef.current = true
    setHasNewBelow(false)
  }

  async function openConversation(id: string) {
    setActiveConvId(id)
    setChatError('')
    setSnapshot(null)
    try {
      const d = await getConversation(id)
      setMessages(d.messages)
    } catch (e) {
      setChatError(e instanceof Error ? e.message : '读取会话失败')
    }
  }

  async function newConversation() {
    setChatError('')
    try {
      const c = await createConversation()
      setActiveConvId(c.id)
      setMessages([])
      setSnapshot(null)
      await refreshConversations()
    } catch (e) {
      setChatError(e instanceof Error ? e.message : '创建会话失败')
    }
  }

  async function sendChat(text?: string) {
    const content = (text ?? chatInput).trim()
    if (!content || chatBusy) return
    setChatInput('')
    setChatError('')
    setChatBusy(true)
    let convId = activeConvId
    try {
      if (!convId) {
        const c = await createConversation()
        convId = c.id
        setActiveConvId(convId)
        await refreshConversations()
      }
      // POST 在服务端跑整轮：先轮询会话，tool 消息落盘后立刻显示，
      // 用户能在右侧活动面板实时看到 Agent 正在执行哪个工具，不用等整轮结束。
      chatPollRef.current = window.setInterval(() => {
        void getConversation(convId)
          .then((d) => setMessages(d.messages))
          .catch(() => undefined)
      }, 800)
      await postChatMessage(convId, content)
    } catch (e) {
      setChatError(e instanceof Error ? e.message : '发送失败')
    } finally {
      if (chatPollRef.current) {
        window.clearInterval(chatPollRef.current)
        chatPollRef.current = null
      }
      setChatBusy(false)
      try {
        const d = await getConversation(convId)
        setMessages(d.messages)
      } catch {
        // 最终刷新失败时保留轮询到的消息，不清空。
      }
    }
  }

  async function convertCurrent() {
    if (!activeConvId || converting) return
    setConverting(true)
    setChatError('')
    try {
      const r = await convertConversation(activeConvId)
      setNotice(`已转成正式诊断，runId：${r.runId}。审批卡在下面聊天里，批准后 Worker 会自动执行并验证。`)
      setRunId(r.runId)
      // 转换后留在对话模式：审批卡和修复验证直接出现在聊天里，不切走。
      await loadRun(r.runId, true)
      startRunPolling(r.runId)
    } catch (e) {
      setChatError(e instanceof Error ? e.message : '转换失败')
    } finally {
      setConverting(false)
    }
  }

  async function loadRun(id: string, reset: boolean) {
    try {
      const snap = await getRun(id)
      setSnapshot(snap)
      if (reset) {
        afterRef.current = 0
        setEvents(snap.steps ?? [])
        afterRef.current = (snap.steps ?? []).reduce((m, s) => Math.max(m, s.seq), 0)
      }
      return snap
    } catch (e) {
      setRunError(e instanceof Error ? e.message : '读取任务失败')
      return null
    }
  }

  function startRunPolling(id: string) {
    if (runPollRef.current) window.clearInterval(runPollRef.current)
    runPollRef.current = window.setInterval(() => {
      void (async () => {
        try {
          const evs = await listRunEvents(id, afterRef.current)
          if (evs.length > 0) {
            afterRef.current = evs.reduce((m, s) => Math.max(m, s.seq), afterRef.current)
            setEvents((prev) => {
              const seen = new Set(prev.map((s) => s.seq))
              return [...prev, ...evs.filter((s) => !seen.has(s.seq))]
            })
          }
          const snap = await loadRun(id, false)
          if (snap && !runStillActive(snap)) {
            if (runPollRef.current) {
              window.clearInterval(runPollRef.current)
              runPollRef.current = null
            }
          }
        } catch {
          // 轮询失败一次不报错，下一轮继续。
        }
      })()
    }, 900)
  }

  async function submitRun() {
    if (runBusy) return
    setRunBusy(true)
    setRunError('')
    setNotice('')
    try {
      const r = await createRun({ namespace: ns, kind, name, question })
      setRunId(r.runId)
      afterRef.current = 0
      setEvents([])
      await loadRun(r.runId, true)
      startRunPolling(r.runId)
    } catch (e) {
      setRunError(e instanceof Error ? e.message : '提交失败')
    } finally {
      setRunBusy(false)
    }
  }

  async function loadRunById() {
    if (!runId) return
    const snap = await loadRun(runId, true)
    if (snap && runStillActive(snap)) startRunPolling(runId)
  }

  async function decide(decision: 'approved' | 'rejected') {
    if (!snapshot?.proposal || deciding) return
    setDeciding(true)
    try {
      const r = await decideApproval(snapshot.id, snapshot.proposal.id, decision)
      setNotice(
        `审批结果：${r.status}${r.alreadyDecided ? '（重复提交，返回当前结果）' : ''}。${
          decision === 'approved' ? 'Worker 会自动执行并验证，请稍等。' : '已拒绝，不会对集群做任何写入。'
        }`,
      )
      const snap = await loadRun(snapshot.id, false)
      if (snap && runStillActive(snap)) startRunPolling(snapshot.id)
    } catch (e) {
      setRunError(e instanceof Error ? e.message : '审批失败')
    } finally {
      setDeciding(false)
    }
  }

  // 右侧活动面板的内容：对话模式汇总会话里所有工具调用，
  // 单次诊断模式用当前任务的步骤；正在执行时另加一行转圈提示。
  const activityItems = mode === 'chat' ? chatActivityItems(messages) : runActivityItems(events)
  const activityRunning = mode === 'chat' ? chatBusy : snapshot ? runStillActive(snapshot) : false
  const runTotalMs = events.reduce((s, e) => s + (e.durationMs > 0 ? e.durationMs : 0), 0)

  return (
    <div className="app">
      <header className="header">
        <div>
          <h1>KubeCause</h1>
          <span className="subtitle">Kubernetes 故障诊断 Agent · 通用 Agent + 对话模式</span>
        </div>
        <nav className="mode-tabs">
          <button className={mode === 'chat' ? 'active' : ''} onClick={() => setMode('chat')}>
            对话模式
          </button>
          <button className={mode === 'run' ? 'active' : ''} onClick={() => setMode('run')}>
            单次诊断
          </button>
        </nav>
      </header>

      {notice && (
        <div className="notice" onClick={() => setNotice('')}>
          {notice}
        </div>
      )}

      <div className="layout">
        <aside className="sidebar">
          <button className="primary" onClick={newConversation}>
            + 新建会话
          </button>
          <h3>会话</h3>
          {conversations.length === 0 && <p className="muted small">还没有会话，点上面新建一个。</p>}
          <ul className="conv-list">
            {conversations.map((c) => (
              <li key={c.id}>
                <button
                  className={activeConvId === c.id ? 'active' : ''}
                  onClick={() => {
                    setMode('chat')
                    void openConversation(c.id)
                  }}
                >
                  {c.title || c.id}
                </button>
              </li>
            ))}
          </ul>
          <div className="side-note">
            <h3>说明</h3>
            <p className="small">
              对话只读：Agent 只查不改，觉得要修就给一张建议卡，点“转成正式诊断”后才走人工审批和执行。
            </p>
            <p className="small">
              系统命名空间可以查（行上有“系统”标记），但不能改——修复校验会直接拒绝。
            </p>
          </div>
        </aside>

        <main className="main" ref={messagesWrapRef} onScroll={onMessagesScroll}>
          {mode === 'chat' ? (
            <>
              {messages.length === 0 ? (
                <div className="empty">
                  <h2>先问一句“当前状态怎么样”</h2>
                  <p className="muted">
                    Agent 会先看集群总览、列出有问题的 Pod；你发现某个 namespace 有问题，再继续追问，
                    它记得上文，不用你重新填 namespace。
                  </p>
                  <div className="chips">
                    {QUICK_QUESTIONS.map((q) => (
                      <button key={q} className="chip" onClick={() => void sendChat(q)} disabled={chatBusy}>
                        {q}
                      </button>
                    ))}
                  </div>
                </div>
              ) : (
                <div className="messages">
                  {messages.map((m) => {
                    // tool 消息不单独占一楼：它的调用已经汇总到右侧活动面板，
                    // 聊天里只在助手消息下留一行工具摘要。
                    if (m.role === 'tool') return null
                    if (m.role === 'user') {
                      return (
                        <div key={m.id} className="msg msg-user">
                          {m.content}
                        </div>
                      )
                    }
                    const calls = parseToolCalls(m)
                    const structured = parseStructured(m)
                    return (
                      <div key={m.id} className="msg msg-assistant">
                        <div className="msg-text">{m.content}</div>
                        <ToolSummary calls={calls} running={chatBusy} />
                        <StructuredView
                          structured={structured}
                          onConvert={convertCurrent}
                          converting={converting}
                        />
                      </div>
                    )
                  })}
                  {chatBusy && (
                    <div className="msg msg-assistant">
                      <div className="msg-text">
                        <span className="spin">◌</span> Agent 正在执行工具，右侧活动面板实时显示…
                      </div>
                    </div>
                  )}
                  {snapshot && (
                    <div className="card run-result">
                      <h3>正式诊断 · {snapshot.id}</h3>
                      <p>
                        任务 {snapshot.namespace}/{snapshot.kind}/{snapshot.name} 状态：
                        <b>{snapshot.status}</b>
                        {snapshot.error && <span className="error-text">　{snapshot.error}</span>}
                      </p>
                      {snapshot.report && <ReportView report={snapshot.report} />}
                      {snapshot.proposal && (
                        <ApprovalCard proposal={snapshot.proposal} onDecide={decide} deciding={deciding} />
                      )}
                      <RepairStatusCard snapshot={snapshot} />
                    </div>
                  )}
                </div>
              )}
              {hasNewBelow && (
                <button className="new-below" onClick={scrollToBottom}>
                  ↓ 有新消息
                </button>
              )}
              {chatError && <div className="error">{chatError}</div>}
              <div className="composer">
                <input
                  value={chatInput}
                  onChange={(e) => setChatInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault()
                      void sendChat()
                    }
                  }}
                  placeholder="问一句：当前状态怎么样？"
                  disabled={chatBusy}
                />
                <button className="primary" onClick={() => void sendChat()} disabled={chatBusy || !chatInput.trim()}>
                  {chatBusy ? '执行中…' : '发送'}
                </button>
                <button onClick={convertCurrent} disabled={!activeConvId || converting}>
                  转成正式诊断
                </button>
              </div>
            </>
          ) : (
            <div className="run-mode">
              <div className="card">
                <h3>单次诊断</h3>
                <label>
                  Namespace <input value={ns} onChange={(e) => setNs(e.target.value)} />
                </label>
                <label>
                  类型{' '}
                  <select value={kind} onChange={(e) => setKind(e.target.value)}>
                    <option>Deployment</option>
                    <option>Pod</option>
                  </select>
                </label>
                <label>
                  名称 <input value={name} onChange={(e) => setName(e.target.value)} />
                </label>
                <label>
                  问题 <input value={question} onChange={(e) => setQuestion(e.target.value)} />
                </label>
                <button className="primary" onClick={submitRun} disabled={runBusy}>
                  {runBusy ? '提交中…' : '提交诊断任务'}
                </button>
              </div>
              <div className="card">
                <label>
                  runId <input value={runId} onChange={(e) => setRunId(e.target.value)} placeholder="run_..." />
                </label>{' '}
                <button onClick={loadRunById}>读取任务</button>
              </div>
              {runError && <div className="error">{runError}</div>}
              {snapshot && (
                <>
                  <p>
                    任务 <code>{snapshot.id}</code> 状态：<b>{snapshot.status}</b>
                    {snapshot.error && <span className="error-text">　{snapshot.error}</span>}
                  </p>
                  <div className="tool-summary">
                    <button className="tool-summary-btn" onClick={() => setRunTimelineOpen(!runTimelineOpen)}>
                      {runStillActive(snapshot)
                        ? '◌ 正在执行工具…'
                        : `已执行 ${events.length} 步${runTotalMs > 0 ? ` · 总耗时 ${runTotalMs}ms` : ' · 耗时 —'}`}
                      <span className="muted"> {runTimelineOpen ? '收起 ▲' : '在右侧活动面板查看 ▼'}</span>
                    </button>
                    {runTimelineOpen && <RunTimeline steps={events} running={runStillActive(snapshot)} />}
                  </div>
                  {snapshot.report && <ReportView report={snapshot.report} />}
                  {snapshot.proposal && (
                    <ApprovalCard proposal={snapshot.proposal} onDecide={decide} deciding={deciding} />
                  )}
                  <RepairStatusCard snapshot={snapshot} />
                </>
              )}
            </div>
          )}
        </main>

        <aside className="activity">
          <h3>Agent 活动</h3>
          <p className="muted small">
            每一步工具一张卡片，可点开展开看参数和结果。正在执行的卡片高亮。
          </p>
          <ActivityPanel items={activityItems} running={activityRunning} />
        </aside>
      </div>
    </div>
  )
}
