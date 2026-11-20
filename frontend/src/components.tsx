// 展示组件：工具时间线、结构化结果（卡片 / 表格）、
// 修复建议卡、审批卡、修复执行与验证卡、右侧活动面板。
// 规则：结构化结果一律按类型渲染成表格和卡片，不要直接贴 JSON，
// 也不要靠猜 JSON 的形状（见 types.buildBlocks）；
// 系统命名空间的行带“系统”徽章；工具失败要显示失败原因，不要吞掉。

import { Component, useState } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { buildBlocks, normalizeReport } from './types'
import type {
  ChangeRequest,
  ClusterOverview,
  ConvertPayload,
  DeploymentRow,
  PodRow,
  Report,
  ResultBlock,
  RunbookSnippet,
  RunSnapshot,
  RunStep,
  SuggestedCreation,
  SuggestedRepair,
  ToolCallRecord,
} from './types'

function argsText(args?: Record<string, string>): string {
  if (!args) return ''
  return Object.entries(args)
    .map(([k, v]) => `${k}=${v}`)
    .join(' ')
}

/** 错误边界（Slice 7）：包住聊天消息区和正式诊断卡片。
 * 某个卡片渲染出错时只显示这个 fallback，不让整个页面白屏。
 * 白屏的根因是 Report 里的 null 数组（已在后端归一化 + 前端兜底），
 * 但以后任何一个渲染组件再抛错，这里都是最后一道防线。 */
export class ErrorBoundary extends Component<{ children: ReactNode }, { hasError: boolean; message: string }> {
  constructor(props: { children: ReactNode }) {
    super(props)
    this.state = { hasError: false, message: '' }
  }
  static getDerivedStateFromError(err: Error) {
    return { hasError: true, message: err.message }
  }
  componentDidCatch(err: Error, info: ErrorInfo) {
    // 只记日志，不上报、不吞掉其他状态。
    console.error('KubeCause 渲染出错（已被错误边界接住）：', err, info)
  }
  render() {
    if (this.state.hasError) {
      return (
        <div className="card error-fallback">
          <h4>这个卡片渲染失败了</h4>
          <p className="small muted">{this.state.message || '未知渲染错误'}</p>
          <p className="small">其他内容不受影响。点下面刷新页面，重新拉一次会话。</p>
          <button onClick={() => window.location.reload()}>刷新页面</button>
        </div>
      )
    }
    return this.props.children
  }
}

// ---------------------------------------------------------------------------
// 轻量 Markdown 渲染（Slice 7，手写，不引入重型 Markdown 库）。
// 助手消息以前是 white-space: pre-wrap 原样显示，
// 模型返回的“- 列表”“**加粗**”全是一坨纯文本，历史记录没法看。
// 这里只支持最常用的四种：**加粗**、 `代码`、- 无序列表、1. 有序列表，
// 再加空行分段。解析失败时 fallback 回纯文本，绝不白屏。
// ---------------------------------------------------------------------------

/** 行内解析：**加粗** 和 `代码`，其他原样返回。 */
function renderInline(text: string, keyPrefix: string): ReactNode[] {
  // 按 **...** 或 `...` 切段，奇数段是标记内容。
  const parts = text.split(/(\*\*[^*]+\*\*|`[^`]+`)/g)
  return parts.map((p, i) => {
    if (p.startsWith('**') && p.endsWith('**') && p.length > 4) {
      return <b key={`${keyPrefix}-${i}`}>{p.slice(2, -2)}</b>
    }
    if (p.startsWith('`') && p.endsWith('`') && p.length > 2) {
      return <code key={`${keyPrefix}-${i}`}>{p.slice(1, -1)}</code>
    }
    return <span key={`${keyPrefix}-${i}`}>{p}</span>
  })
}

/** 助手消息的 Markdown 渲染：分段落、无序列表、有序列表。 */
export function MarkdownText({ text }: { text: string }) {
  let nodes: ReactNode[]
  try {
    nodes = renderMarkdownBlocks(text ?? '')
  } catch {
    // 渲染失败时 fallback 回纯文本，不要白屏。
    return <div className="msg-text">{text}</div>
  }
  return <div className="msg-text">{nodes}</div>
}

function renderMarkdownBlocks(text: string): ReactNode[] {
  const lines = text.split('\n')
  const out: ReactNode[] = []
  let i = 0
  let key = 0
  while (i < lines.length) {
    const line = lines[i]
    if (line.trim() === '') {
      i++
      continue
    }
    // 无序列表：连续的 "- " / "* " / "• " 行成一组。
    if (/^\s*[-*•]\s+/.test(line)) {
      const items: string[] = []
      while (i < lines.length && /^\s*[-*•]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*•]\s+/, ''))
        i++
      }
      out.push(
        <ul key={key++} className="md-list">
          {items.map((it, j) => (
            <li key={j}>{renderInline(it, `ul-${key}-${j}`)}</li>
          ))}
        </ul>,
      )
      continue
    }
    // 有序列表：连续的 "1. " / "1) " 行成一组。
    if (/^\s*\d+[.)]\s+/.test(line)) {
      const items: string[] = []
      while (i < lines.length && /^\s*\d+[.)]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*\d+[.)]\s+/, ''))
        i++
      }
      out.push(
        <ol key={key++} className="md-list">
          {items.map((it, j) => (
            <li key={j}>{renderInline(it, `ol-${key}-${j}`)}</li>
          ))}
        </ol>,
      )
      continue
    }
    // 普通段落。
    out.push(<p key={key++}>{renderInline(line, `p-${key}`)}</p>)
    i++
  }
  return out
}

function parseStepArgs(step: RunStep): Record<string, string> {
  if (!step.args) return {}
  try {
    const v = JSON.parse(step.args) as unknown
    if (v && typeof v === 'object' && !Array.isArray(v)) return v as Record<string, string>
  } catch {
    // args 不是 JSON 就原样显示在摘要里。
  }
  return {}
}

/** 工具执行时间线：对话的 toolCalls 或单次诊断的 steps 共用这套展示。 */
export function ToolTimeline({
  calls,
  running,
}: {
  calls: ToolCallRecord[]
  running?: boolean
}) {
  if (calls.length === 0 && !running) return null
  return (
    <div className="timeline">
      {calls.map((c, i) => (
        <div key={c.id || `${c.name}-${i}`} className={`tl-row ${c.error ? 'tl-error' : ''}`}>
          <span className="tl-dot">{c.error ? '✕' : '✓'}</span>
          <div className="tl-body">
            <div className="tl-head">
              <code>{c.name}</code>
              <span className="tl-args">{argsText(c.args)}</span>
              <span className="tl-dur">{c.durationMs > 0 ? `${c.durationMs}ms` : '—'}</span>
            </div>
            <div className="tl-summary">{c.error ? `失败：${c.error}` : c.summary}</div>
          </div>
        </div>
      ))}
      {running && (
        <div className="tl-row tl-running">
          <span className="tl-dot spin">◌</span>
          <div className="tl-body">
            <div className="tl-head">正在执行工具…</div>
            <div className="tl-summary">Agent 正在调工具查证据，稍等。</div>
          </div>
        </div>
      )}
    </div>
  )
}

/** 单次诊断的时间线：steps 来自 /api/runs/{id}/events 轮询。 */
export function RunTimeline({ steps, running }: { steps: RunStep[]; running: boolean }) {
  if (steps.length === 0 && !running) return null
  return (
    <div className="timeline">
      {steps.map((s) => {
        if (s.kind === 'report') {
          return (
            <div key={s.seq} className="tl-row">
              <span className="tl-dot">✓</span>
              <div className="tl-body">
                <div className="tl-head">
                  <code>诊断报告</code>
                </div>
                <div className="tl-summary">{s.summary}</div>
              </div>
            </div>
          )
        }
        if (s.kind === 'repair') {
          return (
            <div key={s.seq} className="tl-row">
              <span className="tl-dot">✓</span>
              <div className="tl-body">
                <div className="tl-head">
                  <code>修复</code>
                </div>
                <div className="tl-summary">{s.summary}</div>
              </div>
            </div>
          )
        }
        if (s.kind === 'error') {
          return (
            <div key={s.seq} className="tl-row tl-error">
              <span className="tl-dot">✕</span>
              <div className="tl-body">
                <div className="tl-head">
                  <code>出错</code>
                </div>
                <div className="tl-summary">{s.error || s.summary}</div>
              </div>
            </div>
          )
        }
        const args = parseStepArgs(s)
        return (
          <div key={s.seq} className={`tl-row ${s.error ? 'tl-error' : ''}`}>
            <span className="tl-dot">{s.error ? '✕' : '✓'}</span>
            <div className="tl-body">
              <div className="tl-head">
                <code>{s.toolName || s.kind}</code>
                <span className="tl-args">{argsText(args)}</span>
                {s.durationMs > 0 && <span className="tl-dur">{s.durationMs}ms</span>}
              </div>
              <div className="tl-summary">{s.error ? `失败：${s.error}` : s.summary}</div>
            </div>
          </div>
        )
      })}
      {running && (
        <div className="tl-row tl-running">
          <span className="tl-dot spin">◌</span>
          <div className="tl-body">
            <div className="tl-head">正在执行工具…</div>
            <div className="tl-summary">Worker 正在后台跑诊断，时间线每秒刷新一次。</div>
          </div>
        </div>
      )}
    </div>
  )
}

/** 聊天里的一行工具摘要：只留一行，不把完整时间线贴进聊天。
 * 点一下展开看完整时间线，或去右侧活动面板看卡片。 */
export function ToolSummary({ calls, running }: { calls: ToolCallRecord[]; running?: boolean }) {
  const [open, setOpen] = useState(false)
  const done = calls.length
  const totalMs = calls.reduce((s, c) => s + (c.durationMs > 0 ? c.durationMs : 0), 0)
  const failed = calls.filter((c) => c.error).length
  if (done === 0 && !running) return null
  return (
    <div className="tool-summary">
      <button className="tool-summary-btn" onClick={() => setOpen(!open)}>
        {running ? '◌ 正在执行工具…' : `查了 ${done} 个工具`}
        {totalMs > 0 ? ` · 总耗时 ${totalMs}ms` : ' · 耗时 —'}
        {failed > 0 ? ` · ${failed} 个失败` : ''}
        <span className="muted"> {open ? '收起 ▲' : '展开 ▼'}</span>
      </button>
      {open && <ToolTimeline calls={calls} running={running} />}
    </div>
  )
}

/** 右侧活动面板用的一条记录：对话的工具调用和单次诊断的步骤统一成这种形状。 */
export interface ActivityItem {
  id: string
  name: string
  args?: Record<string, string>
  summary: string
  error?: string
  durationMs: number
  running?: boolean
}

export function formatDuration(ms: number): string {
  return ms > 0 ? `${ms}ms` : '—'
}

/** 右侧“Agent 活动”面板：每一步工具一张卡片，可点开展开看详情。
 * 正在执行的卡片高亮带转圈；失败的卡片标红并显示失败原因。 */
export function ActivityPanel({ items, running }: { items: ActivityItem[]; running?: boolean }) {
  const [openIds, setOpenIds] = useState<Record<string, boolean>>({})
  if (items.length === 0 && !running) {
    return (
      <p className="muted small">
        还没有工具调用。问一句“当前状态怎么样”，这里会实时显示 Agent 正在调哪个工具。
      </p>
    )
  }
  const toggle = (id: string) => setOpenIds((p) => ({ ...p, [id]: !p[id] }))
  return (
    <div className="activity-list">
      {items.map((it) => {
        const open = !!openIds[it.id]
        return (
          <div
            key={it.id}
            className={`act-card ${it.error ? 'act-error' : ''} ${it.running ? 'act-running' : ''}`}
          >
            <button className="act-head" onClick={() => toggle(it.id)}>
              <span className={`tl-dot ${it.running ? 'spin' : ''}`}>{it.error ? '✕' : it.running ? '◌' : '✓'}</span>
              <code>{it.name}</code>
              <span className="tl-dur">{formatDuration(it.durationMs)}</span>
              <span className="muted">{open ? '▲' : '▼'}</span>
            </button>
            {open && (
              <div className="act-body">
                {it.args && <div className="tl-args">{argsText(it.args)}</div>}
                <div className="tl-summary">{it.error ? `失败：${it.error}` : it.summary}</div>
              </div>
            )}
          </div>
        )
      })}
      {running && (
        <div className="act-card act-running">
          <div className="act-head">
            <span className="tl-dot spin">◌</span>
            <span>正在执行工具…</span>
          </div>
        </div>
      )}
    </div>
  )
}

function SystemBadge() {
  return <span className="badge-sys">系统</span>
}

export function OverviewCards({ overview }: { overview: ClusterOverview }) {
  // 防御：后端返回的总览缺字段时按 0 显示，不读 undefined 的属性。
  const o = overview ?? {}
  const cards: Array<{ label: string; value: number | string }> = [
    { label: '节点', value: o.nodes ?? 0 },
    { label: '命名空间', value: o.namespaces ?? 0 },
    { label: 'Deployment', value: o.deployments ?? 0 },
    { label: 'Pod', value: o.pods ?? 0 },
    { label: '有问题 Pod', value: o.problemPods ?? 0 },
    { label: '未就绪 Deployment', value: o.notReadyDeployments ?? 0 },
  ]
  return (
    <div className="cards">
      {cards.map((c) => (
        <div key={c.label} className="stat-card">
          <div className="stat-value">{c.value}</div>
          <div className="stat-label">{c.label}</div>
        </div>
      ))}
      {o.systemNamespaces !== undefined && o.systemNamespaces > 0 && (
        <div className="stat-note">
          其中系统命名空间 {o.systemNamespaces} 个
          {o.systemProblemPods !== undefined && o.systemProblemPods > 0
            ? `，系统 Pod 有问题 ${o.systemProblemPods} 个`
            : ''}
        </div>
      )}
    </div>
  )
}

export function PodTable({ rows }: { rows: PodRow[] }) {
  // 长列表默认只显示前 10 行，点开看全部：聊天里先给摘要，不把整张表一次性顶上去。
  const [showAll, setShowAll] = useState(false)
  // 防御：rows 为 null / undefined 时当作空列表，不读 .length。
  const list = rows ?? []
  if (list.length === 0) return <p className="muted">没有符合条件的 Pod。</p>
  const shown = showAll ? list : list.slice(0, 10)
  return (
    <>
      <table className="tbl">
        <thead>
          <tr>
            <th>NAME</th>
            <th>NS</th>
            <th>READY</th>
            <th>STATUS</th>
            <th>RESTARTS</th>
            <th>原因</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((r) => (
            <tr key={`${r.namespace}/${r.name}`}>
              <td>
                <code>{r.name}</code>
                {r.deployment && <div className="muted small">{r.deployment}</div>}
              </td>
              <td>
                {r.namespace} {r.isSystem && <SystemBadge />}
              </td>
              <td>{r.ready}</td>
              <td>{r.phase}</td>
              <td>{r.restarts}</td>
              <td>{r.lastTerminationReason || r.waitingReason || '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {list.length > 10 && (
        <button className="link-btn" onClick={() => setShowAll(!showAll)}>
          {showAll ? '收起' : `展开全部 ${list.length} 行 ▼`}
        </button>
      )}
    </>
  )
}

export function DeploymentTable({ rows }: { rows: DeploymentRow[] }) {
  // 防御：rows 为 null / undefined 时当作空列表。
  const list = rows ?? []
  if (list.length === 0) return <p className="muted">没有符合条件的 Deployment。</p>
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th>NAME</th>
          <th>NS</th>
          <th>副本</th>
          <th>就绪</th>
          <th>可用</th>
        </tr>
      </thead>
      <tbody>
        {list.map((r) => (
          <tr key={`${r.namespace}/${r.name}`}>
            <td>
              <code>{r.name}</code>
            </td>
            <td>
              {r.namespace} {r.isSystem && <SystemBadge />}
            </td>
            <td>{r.replicas}</td>
            <td>{r.readyReplicas}</td>
            <td>{r.availableReplicas}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/** 一个结果块：标题栏永远可见，正文默认按类型折叠一部分。
 * 总览、报告、修复建议默认展开；Pod 表格、事件列表默认展开但内部只显示前几行，
 * 点开看全部。不知道的类型不渲染，直接返回 null，不白屏。
 * onConvert 带可选的转换目标（Slice 10）：建议卡上的按钮把这张卡的
 * 目标和 Patches（或创建规格）一起传过去，后端优先用它们。 */
function ResultBlockView({ block, onConvert, converting }: { block: ResultBlock; onConvert?: (payload?: ConvertPayload) => void; converting?: boolean }) {
  // 总览和修复建议是本轮的核心结论，默认展开；长列表默认展开但内部折叠行数。
  const [open, setOpen] = useState(block.kind !== 'events')
  const [showAllEvents, setShowAllEvents] = useState(false)
  let body: ReactNode = null
  let summary = ''
  switch (block.kind) {
    case 'overview':
      body = <OverviewCards overview={block.data as ClusterOverview} />
      summary = '集群状态总览'
      break
    case 'podList': {
      const pods = (block.data as { pods?: PodRow[] }).pods ?? []
      summary = `有问题 Pod ${pods.length} 个`
      body = <PodTable rows={pods} />
      break
    }
    case 'deploymentList': {
      const deps = (block.data as { deployments?: DeploymentRow[] }).deployments ?? []
      summary = `${deps.length} 个 Deployment`
      body = <DeploymentTable rows={deps} />
      break
    }
    case 'events': {
      const evs = (block.data as { events?: Array<{ reason: string; message: string; object: string }> }).events ?? []
      summary = `${evs.length} 条相关事件`
      const shown = showAllEvents ? evs : evs.slice(0, 5)
      body = (
        <>
          <ul className="ev-list">
            {shown.map((e, i) => (
              <li key={i}>
                <b>{e.reason}</b> {e.message}
                {e.object && <span className="muted">（{e.object}）</span>}
              </li>
            ))}
          </ul>
          {evs.length > 5 && (
            <button className="link-btn" onClick={() => setShowAllEvents(!showAllEvents)}>
              {showAllEvents ? '收起' : `展开全部 ${evs.length} 条 ▼`}
            </button>
          )}
        </>
      )
      break
    }
    case 'report': {
      const rep = normalizeReport(block.data)
      body = <ReportView report={rep} />
      summary = rep.cause || '诊断报告'
      break
    }
    case 'repairSuggestion': {
      const sr = block.data as SuggestedRepair
      if (!sr || !sr.namespace || !sr.deployment) return null
      // 通用 Patch 摘要：有 patches 时写“n 个 Patch”，
      // 老卡没有 patches 时沿用旧的“旧值 → 新值”写法。
      summary =
        sr.patches && sr.patches.length > 0
          ? `${sr.namespace}/${sr.deployment} · ${sr.patches.length} 个 Patch`
          : `${sr.namespace}/${sr.deployment} ${sr.oldValue ?? ''} → ${sr.newValue ?? ''}`
      body = <RepairSuggestionCard sr={sr} onConvert={onConvert} converting={converting} />
      break
    }
    case 'creationSuggestion': {
      const sc = block.data as SuggestedCreation
      if (!sc || !sc.namespace || !sc.name || !sc.image) return null
      summary = `${sc.namespace}/${sc.name} · ${sc.image} · ${sc.replicas ?? 1} 个副本`
      body = <CreationSuggestionCard sc={sc} onConvert={onConvert} converting={converting} />
      break
    }
    case 'runbooks': {
      // 参考了哪篇手册：每个片段带标题和关键句，点开看全文要点。
      // runbooks 是 RAG 知识库（runbooks/*.md，FTS5 索引），不是为某个案例写的特例。
      const snippets = (block.data ?? []) as RunbookSnippet[]
      summary = `${snippets.length} 篇手册`
      body = (
        <ul className="ev-list">
          {snippets.map((s, i) => (
            <li key={i}>
              <b>{s.title || '排障手册'}</b>
              {s.source && <span className="muted">（{s.source}）</span>}
              {s.content && <div className="muted small">{s.content}</div>}
            </li>
          ))}
        </ul>
      )
      break
    }
    case 'text':
      body = <p>{String(block.data)}</p>
      summary = String(block.data).slice(0, 40)
      break
    default:
      return null
  }
  return (
    <div className="result-block">
      <button className="result-head" onClick={() => setOpen(!open)}>
        <b>{block.title}</b>
        <span className="muted small">{summary}</span>
        <span className="muted">{open ? '▲' : '▼'}</span>
      </button>
      {open && <div className="result-body">{body}</div>}
    </div>
  )
}

/** 结构化结果渲染：先按类型包成结果块（types.buildBlocks），再按块渲染。
 * 审批卡、修复建议卡的“创建可审批的修复任务”按钮只在有建议卡时显示，
 * 不只在最新一条消息上才显示，翻历史、刷新后按钮还在。 */
export function StructuredView({
  structured,
  onConvert,
  converting,
}: {
  structured: Record<string, unknown>
  onConvert?: (payload?: ConvertPayload) => void
  converting?: boolean
}) {
  const blocks = buildBlocks(structured)
  if (blocks.length === 0) return null
  return (
    <div className="structured">
      {blocks.map((b, i) => (
        <ResultBlockView key={`${b.kind}-${i}`} block={b} onConvert={onConvert} converting={converting} />
      ))}
    </div>
  )
}

/** 把一条 Patch 的 path 翻译成人话，再配上新值展示。
 * 旧值只有老卡才有（field=oldValue->newValue），通用 Patch 只有新值，
 * 读不到旧值时只显示新值，不编一个旧值出来。 */
export function patchLabel(path: string): string {
  if (path === '/spec/replicas') return '副本数'
  if (path.endsWith('/image')) return '镜像'
  if (path.endsWith('/resources/limits/memory')) return '内存 limit'
  if (path.endsWith('/resources/requests/memory')) return '内存 request'
  if (path.endsWith('/resources/limits/cpu')) return 'CPU limit'
  if (path.endsWith('/resources/requests/cpu')) return 'CPU request'
  return path
}

function patchValueText(v: unknown): string {
  if (v === null || v === undefined) return '—'
  if (typeof v === 'string') return v
  return String(v)
}

/** 通用 Patch 列表展示（Slice 9）：每行一个 Patch，
 * 字段中文名 + path + 旧值 → 新值（旧值读不到只显示新值）。
 * 老的 memory-only 卡没有 patches 时，按旧字段显示一行内存变更。 */
export function PatchList({
  patches,
  oldValue,
  newValue,
}: {
  patches?: Array<{ op: string; path: string; value: unknown }>
  oldValue?: string
  newValue?: string
}) {
  const list = patches ?? []
  if (list.length === 0) {
    return (
      <p>
        {oldValue ?? ''} → <b>{newValue ?? ''}</b>
      </p>
    )
  }
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th>字段</th>
          <th>路径</th>
          <th>新值</th>
        </tr>
      </thead>
      <tbody>
        {list.map((p, i) => (
          <tr key={`${p.path}-${i}`}>
            <td>{patchLabel(p.path)}</td>
            <td>
              <code>{p.path}</code>
            </td>
            <td>
              {/* 只有单条 Patch 且老卡给了旧值时才显示旧值 → 新值，
                  否则只显示新值：旧值要从集群现状读，前端手里没有，
                  不能编。 */}
              {list.length === 1 && oldValue ? `${oldValue} → ` : ''}
              <b>{patchValueText(p.value)}</b>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/** 修复建议卡（对话模式）：只是一张卡，不会自动执行。
 * 转换按钮把这张卡的目标和 Patches 一起传过去（Slice 10），
 * 后端优先用它们建正式诊断，不再去历史里猜目标。 */
export function RepairSuggestionCard({
  sr,
  onConvert,
  converting,
}: {
  sr: SuggestedRepair
  onConvert?: (payload?: ConvertPayload) => void
  converting?: boolean
}) {
  return (
    <div className="card repair-card">
      <h4>修复建议（仅建议，未执行）</h4>
      <p>
        目标：{sr.namespace}/{sr.deployment} 容器 {sr.container}
        <br />
        理由：{sr.reason}
      </p>
      <PatchList patches={sr.patches} oldValue={sr.oldValue} newValue={sr.newValue} />
      <p className="muted small">
        这只是一张建议卡，不会自动改集群。想走正式流程，点“创建可审批的修复任务”，
        生成一份带提案的诊断任务，人工审批后才会执行。
      </p>
      {onConvert && (
        <button
          onClick={() =>
            onConvert({
              namespace: sr.namespace,
              kind: 'Deployment',
              name: sr.deployment,
              container: sr.container,
              patches: sr.patches ?? [],
            })
          }
          disabled={converting}
        >
          {converting ? '创建中…' : '创建可审批的修复任务'}
        </button>
      )}
    </div>
  )
}

/** 创建建议卡（对话模式，Slice 10）：和修复建议卡长得一样，
 * 只是标题和字段不同，动作从“改”换成“创建”。
 * 转换按钮把创建规格一起传过去，后端直接校验、生成创建提案，
 * 不用重新诊断一遍。 */
export function CreationSuggestionCard({
  sc,
  onConvert,
  converting,
}: {
  sc: SuggestedCreation
  onConvert?: (payload?: ConvertPayload) => void
  converting?: boolean
}) {
  const resources = [
    sc.memoryLimit ? `内存 limit ${sc.memoryLimit}` : '',
    sc.memoryRequest ? `内存 request ${sc.memoryRequest}` : '',
    sc.cpuLimit ? `CPU limit ${sc.cpuLimit}` : '',
    sc.cpuRequest ? `CPU request ${sc.cpuRequest}` : '',
  ].filter(Boolean)
  return (
    <div className="card repair-card">
      <h4>创建建议（仅建议，未执行）</h4>
      <p>
        目标：{sc.namespace}/{sc.name}
        <br />
        镜像：{sc.image}　副本数：{sc.replicas ?? 1}
        {sc.container && sc.container !== sc.name ? (
          <>
            <br />
            容器：{sc.container}
          </>
        ) : null}
        {resources.length > 0 ? (
          <>
            <br />
            资源：{resources.join('、')}
          </>
        ) : null}
        {sc.reason ? (
          <>
            <br />
            理由：{sc.reason}
          </>
        ) : null}
      </p>
      <p className="muted small">
        这只是一张建议卡，不会自动创建。想走正式流程，点“创建可审批的创建任务”，
        生成一份带提案的创建任务，人工审批后才会执行。
      </p>
      {onConvert && (
        <button
          onClick={() =>
            onConvert({
              namespace: sc.namespace,
              kind: 'Deployment',
              name: sc.name,
              creation: sc,
            })
          }
          disabled={converting}
        >
          {converting ? '创建中…' : '创建可审批的创建任务'}
        </button>
      )}
    </div>
  )
}

/** 诊断报告展示：原因、置信度、事实、建议、待确认。
 * 防御：报告字段为 null 时（历史库里的旧报告）先归一化成空数组，
 * 再读 .length，绝不因为一个 null 字段让整个页面白屏。 */
export function ReportView({ report }: { report: Report }) {
  const rep = normalizeReport(report)
  const facts = rep.facts ?? []
  const recommendations = rep.recommendations ?? []
  const openQuestions = rep.openQuestions ?? []
  const evidenceIds = rep.evidenceIds ?? []
  return (
    <div className="card">
      <h4>
        诊断报告{' '}
        <span className={`conf conf-${rep.confidence || 'medium'}`}>置信度 {rep.confidence || 'medium'}</span>
      </h4>
      <p>
        <b>原因：</b>
        {rep.cause || '暂无结论'}
      </p>
      {facts.length > 0 && (
        <>
          <h5>事实依据</h5>
          <ul>
            {facts.map((f, i) => (
              <li key={i}>{f}</li>
            ))}
          </ul>
        </>
      )}
      {recommendations.length > 0 && (
        <>
          <h5>建议</h5>
          <ul>
            {recommendations.map((r, i) => (
              <li key={i}>{r}</li>
            ))}
          </ul>
        </>
      )}
      {openQuestions.length > 0 && (
        <>
          <h5>还没确认</h5>
          <ul>
            {openQuestions.map((q, i) => (
              <li key={i}>{q}</li>
            ))}
          </ul>
        </>
      )}
      {evidenceIds.length > 0 && (
        <p className="muted small">证据：{evidenceIds.join('、')}</p>
      )}
      {rep.suggestedCreation && (
        <p className="muted small">
          创建建议：{rep.suggestedCreation.namespace}/{rep.suggestedCreation.name}，
          镜像 {rep.suggestedCreation.image}，{rep.suggestedCreation.replicas ?? 1} 个副本
        </p>
      )}
    </div>
  )
}

/** 审批卡：有提案时才显示，批准/拒绝调审批接口，重复提交返回当前结果。
 * 创建提案（Slice 10）按创建规格展示（镜像、副本数、资源），
 * 不按 Patch 列表展示——创建没有 Patch，只有要创建的东西。 */
export function ApprovalCard({
  proposal,
  onDecide,
  deciding,
}: {
  proposal: ChangeRequest
  onDecide: (decision: 'approved' | 'rejected') => void
  deciding: boolean
}) {
  const pending = proposal.status === 'pending'
  const isCreate = proposal.kind === 'create' && proposal.creation
  const creation = proposal.creation
  const resources = creation
    ? [
        creation.memoryLimit ? `内存 limit ${creation.memoryLimit}` : '',
        creation.memoryRequest ? `内存 request ${creation.memoryRequest}` : '',
        creation.cpuLimit ? `CPU limit ${creation.cpuLimit}` : '',
        creation.cpuRequest ? `CPU request ${creation.cpuRequest}` : '',
      ].filter(Boolean)
    : []
  return (
    <div className="card approve">
      <h4>{isCreate ? '创建审批卡' : '审批卡'}（人工审批后才执行）</h4>
      <p>
        目标：{proposal.namespace}/{proposal.deployment}
        {!isCreate && proposal.container ? ` 容器 ${proposal.container}` : ''}
        <br />
        提案状态：{proposal.status}
        {proposal.approver ? `，审批人：${proposal.approver}` : ''}
      </p>
      {isCreate && creation ? (
        <p>
          镜像：{creation.image}　副本数：{creation.replicas ?? 1}
          {resources.length > 0 ? (
            <>
              <br />
              资源：{resources.join('、')}
            </>
          ) : null}
          {creation.reason ? (
            <>
              <br />
              理由：{creation.reason}
            </>
          ) : null}
        </p>
      ) : (
        <PatchList patches={proposal.patches} oldValue={proposal.oldValue} newValue={proposal.newValue} />
      )}
      <button onClick={() => onDecide('approved')} disabled={!pending || deciding}>
        批准
      </button>{' '}
      <button onClick={() => onDecide('rejected')} disabled={!pending || deciding}>
        拒绝
      </button>
      {!pending && <p className="muted small">已决定，重复提交会返回当前结果，不会重复执行。</p>}
    </div>
  )
}

/** 修复执行与验证：执行结果和验证结果分开显示，写入成功不等于验证通过。 */
export function RepairStatusCard({ snapshot }: { snapshot: RunSnapshot }) {
  const p = snapshot.proposal
  if (!p || (p.executionStatus === 'not_started' && p.verificationStatus === 'not_started')) {
    return null
  }
  const exec = snapshot.executionDetail as Record<string, unknown> | undefined
  const ver = snapshot.verificationDetail as Record<string, unknown> | undefined
  return (
    <div className="card">
      <h4>修复执行 / 验证</h4>
      <p>
        执行：<b>{p.executionStatus}</b>　验证：<b>{p.verificationStatus}</b>
      </p>
      {exec && (
        <p className="small">
          写入：{exec.beforeValue ? `${String(exec.beforeValue)} → ` : ''}
          {String(exec.afterValue ?? '')}
          {exec.dryRunOk ? '，dry-run 通过' : ''}
          {typeof exec.satisfied === 'boolean' ? (exec.satisfied ? '，读回确认已生效' : '，读回未生效') : ''}
        </p>
      )}
      {ver && (
        <p className="small">
          验证：observedGeneration {String(ver.observedGeneration ?? '')}/
          {String(ver.generation ?? '')}，新 Pod{' '}
          {String(ver.newPodName ?? '—')} Ready=
          {String(ver.newPodReady ?? '—')}，重启次数 {String(ver.restartCountBefore ?? '')} →{' '}
          {String(ver.restartCountAfter ?? '')}
          {ver.restartsGrew ? '（重启次数继续增长，修复未稳住）' : ''}
        </p>
      )}
    </div>
  )
}
