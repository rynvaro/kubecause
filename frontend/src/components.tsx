// 展示组件：工具时间线、结构化结果（卡片 / 表格）、
// 修复建议卡、审批卡、修复执行与验证卡、右侧活动面板。
// 规则：结构化结果一律按类型渲染成表格和卡片，不要直接贴 JSON，
// 也不要靠猜 JSON 的形状（见 types.buildBlocks）；
// 系统命名空间的行带“系统”徽章；工具失败要显示失败原因，不要吞掉。

import { useState } from 'react'
import type { ReactNode } from 'react'
import { buildBlocks } from './types'
import type {
  ChangeRequest,
  ClusterOverview,
  DeploymentRow,
  PodRow,
  Report,
  ResultBlock,
  RunSnapshot,
  RunStep,
  SuggestedRepair,
  ToolCallRecord,
} from './types'

function argsText(args?: Record<string, string>): string {
  if (!args) return ''
  return Object.entries(args)
    .map(([k, v]) => `${k}=${v}`)
    .join(' ')
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
  const cards: Array<{ label: string; value: number | string }> = [
    { label: '节点', value: overview.nodes },
    { label: '命名空间', value: overview.namespaces },
    { label: 'Deployment', value: overview.deployments },
    { label: 'Pod', value: overview.pods },
    { label: '有问题 Pod', value: overview.problemPods },
    { label: '未就绪 Deployment', value: overview.notReadyDeployments },
  ]
  return (
    <div className="cards">
      {cards.map((c) => (
        <div key={c.label} className="stat-card">
          <div className="stat-value">{c.value}</div>
          <div className="stat-label">{c.label}</div>
        </div>
      ))}
      {overview.systemNamespaces !== undefined && overview.systemNamespaces > 0 && (
        <div className="stat-note">
          其中系统命名空间 {overview.systemNamespaces} 个
          {overview.systemProblemPods !== undefined && overview.systemProblemPods > 0
            ? `，系统 Pod 有问题 ${overview.systemProblemPods} 个`
            : ''}
        </div>
      )}
    </div>
  )
}

export function PodTable({ rows }: { rows: PodRow[] }) {
  // 长列表默认只显示前 10 行，点开看全部：聊天里先给摘要，不把整张表一次性顶上去。
  const [showAll, setShowAll] = useState(false)
  if (rows.length === 0) return <p className="muted">没有符合条件的 Pod。</p>
  const shown = showAll ? rows : rows.slice(0, 10)
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
      {rows.length > 10 && (
        <button className="link-btn" onClick={() => setShowAll(!showAll)}>
          {showAll ? '收起' : `展开全部 ${rows.length} 行 ▼`}
        </button>
      )}
    </>
  )
}

export function DeploymentTable({ rows }: { rows: DeploymentRow[] }) {
  if (rows.length === 0) return <p className="muted">没有符合条件的 Deployment。</p>
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
        {rows.map((r) => (
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
 * 点开看全部。不知道的类型不渲染，直接返回 null，不白屏。 */
function ResultBlockView({ block, onConvert, converting }: { block: ResultBlock; onConvert?: () => void; converting?: boolean }) {
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
    case 'report':
      body = <ReportView report={block.data as Report} />
      summary = (block.data as Report).cause
      break
    case 'repairSuggestion': {
      const sr = block.data as SuggestedRepair
      summary = `${sr.namespace}/${sr.deployment} ${sr.oldValue} → ${sr.newValue}`
      body = <RepairSuggestionCard sr={sr} onConvert={onConvert} converting={converting} />
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
 * 审批卡、修复建议卡的“转成正式诊断”按钮只要有建议卡就一直显示，
 * 不只在最新一条消息上才显示，翻历史、刷新后按钮还在。 */
export function StructuredView({
  structured,
  onConvert,
  converting,
}: {
  structured: Record<string, unknown>
  onConvert?: () => void
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

/** 修复建议卡（对话模式）：只是一张卡，不会自动执行。 */
export function RepairSuggestionCard({
  sr,
  onConvert,
  converting,
}: {
  sr: SuggestedRepair
  onConvert?: () => void
  converting?: boolean
}) {
  return (
    <div className="card repair-card">
      <h4>修复建议（仅建议，未执行）</h4>
      <p>
        目标：{sr.namespace}/{sr.deployment} 容器 {sr.container}
        <br />
        字段：{sr.field}，{sr.oldValue} → <b>{sr.newValue}</b>
        <br />
        理由：{sr.reason}
      </p>
      <p className="muted small">
        这只是一张建议卡。想走正式流程，点“转成正式诊断”，生成提案后人工审批才会执行。
      </p>
      {onConvert && (
        <button onClick={onConvert} disabled={converting}>
          {converting ? '转换中…' : '转成正式诊断'}
        </button>
      )}
    </div>
  )
}

/** 诊断报告展示：原因、置信度、事实、建议、待确认。 */
export function ReportView({ report }: { report: Report }) {
  return (
    <div className="card">
      <h4>
        诊断报告{' '}
        <span className={`conf conf-${report.confidence}`}>置信度 {report.confidence}</span>
      </h4>
      <p>
        <b>原因：</b>
        {report.cause}
      </p>
      {report.facts.length > 0 && (
        <>
          <h5>事实依据</h5>
          <ul>
            {report.facts.map((f, i) => (
              <li key={i}>{f}</li>
            ))}
          </ul>
        </>
      )}
      {report.recommendations.length > 0 && (
        <>
          <h5>建议</h5>
          <ul>
            {report.recommendations.map((r, i) => (
              <li key={i}>{r}</li>
            ))}
          </ul>
        </>
      )}
      {report.openQuestions.length > 0 && (
        <>
          <h5>还没确认</h5>
          <ul>
            {report.openQuestions.map((q, i) => (
              <li key={i}>{q}</li>
            ))}
          </ul>
        </>
      )}
      {report.evidenceIds.length > 0 && (
        <p className="muted small">证据：{report.evidenceIds.join('、')}</p>
      )}
    </div>
  )
}

/** 审批卡：有提案时才显示，批准/拒绝调审批接口，重复提交返回当前结果。 */
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
  return (
    <div className="card approve">
      <h4>审批卡（人工审批后才执行）</h4>
      <p>
        目标：{proposal.namespace}/{proposal.deployment} 容器 {proposal.container}
        <br />
        内存 limit {proposal.oldValue} → <b>{proposal.newValue}</b>
        <br />
        提案状态：{proposal.status}
        {proposal.approver ? `，审批人：${proposal.approver}` : ''}
      </p>
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
          写入：{String(exec.beforeValue ?? '')} → {String(exec.afterValue ?? '')}
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
