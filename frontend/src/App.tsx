// KubeCause 前端主界面（Slice 5）。
//
// 布局：左侧会话列表，中间聊天 / 单次诊断，右侧详情。
// 两种模式共用同一套后端：对话模式是一问一答（只读），
// 单次诊断是填表单、等报告、可审批、可执行、可验证。
//
// 实时显示用轮询，不用 WebSocket：
//   - 对话：POST /messages 在服务端跑整轮，期间 tool 消息会陆续落盘，
//     前端每 800ms 轮询一次会话，就能实时看到“正在执行哪个工具”；
//   - 单次诊断：每 900ms 轮询 /api/runs/{id}/events?after=N 追加时间线，
//     同时刷新任务快照看状态、报告和提案。
// 轮询比 WebSocket 简单，够用；后端不用为实时显示改协议。

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
  ApprovalCard,
  RepairStatusCard,
  ReportView,
  RunTimeline,
  StructuredView,
  ToolTimeline,
} from './components'
import {
  parseStructured,
  parseToolCalls,
} from './types'
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

  const afterRef = useRef(0)
  const chatPollRef = useRef<number | null>(null)
  const runPollRef = useRef<number | null>(null)
  const messagesRef = useRef<ConversationMessage[]>([])
  messagesRef.current = messages

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

  async function openConversation(id: string) {
    setActiveConvId(id)
    setChatError('')
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
      // 用户能看到 Agent 正在执行哪个工具，不用等整轮结束。
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
      setNotice(`已转成正式诊断，runId：${r.runId}。下面可以审批和执行。`)
      setRunId(r.runId)
      setMode('run')
      await loadRun(r.runId, true)
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

  const lastAssistant = [...messages].reverse().find((m) => m.role === 'assistant')

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

        <main className="main">
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
                  {messages.map((m, i) => {
                    if (m.role === 'user') {
                      return (
                        <div key={m.id} className="msg msg-user">
                          {m.content}
                        </div>
                      )
                    }
                    if (m.role === 'tool') {
                      const calls = parseToolCalls(m)
                      const isLast = i === messages.length - 1
                      return (
                        <div key={m.id} className="msg msg-tool">
                          <ToolTimeline calls={calls} running={chatBusy && isLast} />
                          <div className="tl-summary">{m.content}</div>
                        </div>
                      )
                    }
                    const calls = parseToolCalls(m)
                    const structured = parseStructured(m)
                    const isLatest = lastAssistant?.id === m.id
                    return (
                      <div key={m.id} className="msg msg-assistant">
                        <div className="msg-text">{m.content}</div>
                        <ToolTimeline calls={calls} />
                        <StructuredView
                          structured={structured}
                          onConvert={isLatest ? convertCurrent : undefined}
                          converting={converting}
                        />
                      </div>
                    )
                  })}
                  {chatBusy && (
                    <div className="msg msg-assistant">
                      <div className="msg-text">
                        <span className="spin">◌</span> Agent 正在执行工具…
                      </div>
                    </div>
                  )}
                </div>
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
                  <h3>工具执行时间线</h3>
                  <RunTimeline steps={events} running={runStillActive(snapshot)} />
                  {snapshot.report && <ReportView report={snapshot.report} />}
                </>
              )}
            </div>
          )}
        </main>

        <aside className="detail">
          <h3>详情</h3>
          {snapshot ? (
            <>
              <p className="small">
                任务 {snapshot.id}
                <br />
                {snapshot.namespace}/{snapshot.kind}/{snapshot.name}
                <br />
                状态：<b>{snapshot.status}</b>
              </p>
              {snapshot.report && <ReportView report={snapshot.report} />}
              {snapshot.proposal && (
                <ApprovalCard proposal={snapshot.proposal} onDecide={decide} deciding={deciding} />
              )}
              <RepairStatusCard snapshot={snapshot} />
            </>
          ) : (
            <p className="muted small">
              这里显示当前任务的诊断报告、审批卡和修复验证结果。
              <br />
              <br />
              在对话里点“转成正式诊断”，或在“单次诊断”里提交一个任务，这里就会出现详情。
            </p>
          )}
        </aside>
      </div>
    </div>
  )
}
