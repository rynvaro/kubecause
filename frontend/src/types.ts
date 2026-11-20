// 前后端共用的类型定义，和 Go 后端返回的 JSON 字段一一对应。
// 后端改了字段名这里也要跟着改，不要各写各的。

export interface SuggestedRepair {
  namespace: string
  deployment: string
  container: string
  field: string
  oldValue: string
  newValue: string
  reason: string
}

export interface Report {
  facts: string[] | null
  cause: string
  evidenceIds: string[] | null
  recommendations: string[] | null
  openQuestions: string[] | null
  confidence: string
  suggestedRepair?: SuggestedRepair | null
}

// 排障手册片段：search_runbooks 返回的 Data 是 {query, snippets}，
// 每个片段带标题、来源文件和关键句，前端按这个渲染“参考了哪篇手册”。
export interface RunbookSnippet {
  title: string
  source: string
  content: string
}

export interface ChangeRequest {
  id: string
  runId: string
  namespace: string
  deployment: string
  container: string
  oldValue: string
  newValue: string
  deploymentUid?: string
  status: string
  approver?: string
  executionStatus: string
  verificationStatus: string
  executionDetail?: string
  verificationDetail?: string
  createdAt: string
  updatedAt: string
}

export interface RunStep {
  id: number
  runId: string
  seq: number
  kind: string
  toolName?: string
  callId?: string
  args?: string
  result?: string
  summary?: string
  error?: string
  durationMs: number
  retryCount: number
  createdAt: string
}

export interface RunSnapshot {
  id: string
  namespace: string
  kind: string
  name: string
  question: string
  status: string
  maxSteps: number
  timeoutSeconds: number
  report?: Report | null
  error?: string
  steps: RunStep[]
  stepCount: number
  proposal?: ChangeRequest | null
  repairStatus: string
  verificationStatus: string
  executionDetail?: unknown
  verificationDetail?: unknown
  createdAt: string
  updatedAt: string
}

export interface ToolCallRecord {
  id: string
  name: string
  args: Record<string, string>
  summary: string
  error?: string
  // 本次工具调用的耗时（毫秒），后端执行前后计时；0 表示快到不足 1ms，
  // 前端显示“—”，不编一个数字。
  durationMs: number
}

export interface ChatReply {
  reply: string
  toolCalls: ToolCallRecord[]
  structured?: Record<string, unknown>
  suggestedRepair?: SuggestedRepair | null
}

export interface ConversationMessage {
  id: number
  conversationId: string
  role: 'user' | 'assistant' | 'tool'
  content: string
  toolCalls?: string
  structured?: string
  createdAt: string
}

export interface Conversation {
  id: string
  title: string
  createdAt: string
  updatedAt: string
}

// 工具返回的结构化行：Pod / Deployment 表格共用 isSystem 标记，
// 系统命名空间的行前端显示“系统”徽章。
export interface PodRow {
  name: string
  namespace: string
  isSystem?: boolean
  phase: string
  ready: string
  restarts: number
  deployment: string
  lastTerminationReason?: string
  waitingReason?: string
}

export interface DeploymentRow {
  name: string
  namespace: string
  isSystem?: boolean
  replicas: number
  readyReplicas: number
  updatedReplicas: number
  availableReplicas: number
}

export interface ClusterOverview {
  nodes: number
  namespaces: number
  systemNamespaces?: number
  deployments: number
  pods: number
  problemPods: number
  systemProblemPods?: number
  notReadyDeployments: number
}

/** 安全地把可能是 JSON 字符串的值解析成对象，解析失败返回原值。 */
export function parseJSONLoose(v: unknown): unknown {
  if (typeof v !== 'string') return v
  if (!v) return null
  try {
    return JSON.parse(v)
  } catch {
    return v
  }
}

/** 从一条会话消息里解析出本轮调过的工具记录。 */
export function parseToolCalls(msg: ConversationMessage): ToolCallRecord[] {
  const v = parseJSONLoose(msg.toolCalls)
  if (Array.isArray(v)) return v as ToolCallRecord[]
  return []
}

/** 从一条会话消息里解析出结构化结果。 */
export function parseStructured(msg: ConversationMessage): Record<string, unknown> {
  const v = parseJSONLoose(msg.structured)
  if (v && typeof v === 'object' && !Array.isArray(v)) return v as Record<string, unknown>
  return {}
}

// ---------------------------------------------------------------------------
// 结果块（Slice 6）：后端返回的结构化结果按类型包一层再渲染。
//
// 为什么要包一层：大模型返回是会变的，有时是列表（Pod 表格），
// 有时是一个对象（集群总览），有时是一段文字。前端不能靠猜 JSON 的形状
// 来渲染，猜错了就显示不出来。做法是按 key 映射到一个固定类型，
// 每种类型对应一个渲染组件；不知道的类型不渲染成表格，只 fallback 成文字，
// 绝不白屏。工具名原样当 key 的原始结果（如 get_pod、get_events）
// 是 canonical key 的重复，不再单独成块，避免同一份数据出现两次。
// ---------------------------------------------------------------------------

export type ResultBlockKind =
  | 'overview'
  | 'podList'
  | 'deploymentList'
  | 'events'
  | 'report'
  | 'repairSuggestion'
  | 'runbooks'
  | 'text'

export interface ResultBlock {
  kind: ResultBlockKind
  title: string
  data: unknown
}

function asPods(v: unknown): PodRow[] | null {
  if (!v || typeof v !== 'object') return null
  const o = v as { pods?: unknown }
  if (!Array.isArray(o.pods)) return null
  return o.pods as PodRow[]
}

function asDeployments(v: unknown): DeploymentRow[] | null {
  if (!v || typeof v !== 'object') return null
  const o = v as { deployments?: unknown }
  if (!Array.isArray(o.deployments)) return null
  return o.deployments as DeploymentRow[]
}

/** 把可能为 null 的报告字段补成空数组再渲染。
 * 后端 Slice 7 起保证序列化出来是 [] 而不是 null，
 * 但历史数据库里还可能躺着旧的 null 报告，前端这里再兜一层，
 * 读 .length 永远不会抛错，白屏的根因就被两头堵住了。 */
export function normalizeReport(v: unknown): Report {
  const r = (v ?? {}) as Partial<Report>
  return {
    facts: Array.isArray(r.facts) ? r.facts : [],
    cause: r.cause ?? '',
    evidenceIds: Array.isArray(r.evidenceIds) ? r.evidenceIds : [],
    recommendations: Array.isArray(r.recommendations) ? r.recommendations : [],
    openQuestions: Array.isArray(r.openQuestions) ? r.openQuestions : [],
    confidence: r.confidence ?? 'medium',
    suggestedRepair: r.suggestedRepair ?? null,
  }
}

/** 把后端 Structured 的 key 映射成带类型的结果块列表。 */
export function buildBlocks(structured: Record<string, unknown> | null | undefined): ResultBlock[] {
  const blocks: ResultBlock[] = []
  if (!structured || typeof structured !== 'object') return blocks
  if (structured.overview && typeof structured.overview === 'object') {
    blocks.push({ kind: 'overview', title: '集群总览', data: structured.overview })
  }
  const podData = (structured.problemPods ?? structured.pods) as unknown
  const pods = asPods(podData)
  if (pods) {
    blocks.push({ kind: 'podList', title: '有问题的 Pod', data: { pods } })
  } else if (structured.pod && typeof structured.pod === 'object') {
    // 单个 Pod（桩模型深挖路径）：包成一行表格，不再单独写一套展示。
    blocks.push({ kind: 'podList', title: 'Pod', data: { pods: [structured.pod as PodRow] } })
  }
  const depData = structured.deployments as unknown
  const deps = asDeployments(depData)
  if (deps) {
    blocks.push({ kind: 'deploymentList', title: 'Deployment', data: { deployments: deps } })
  } else if (structured.deployment && typeof structured.deployment === 'object') {
    blocks.push({
      kind: 'deploymentList',
      title: 'Deployment',
      data: { deployments: [structured.deployment as DeploymentRow] },
    })
  }
  const evData = structured.events as { events?: unknown } | undefined
  if (evData && Array.isArray(evData.events) && evData.events.length > 0) {
    blocks.push({ kind: 'events', title: '相关事件', data: evData })
  }
  if (structured.report && typeof structured.report === 'object') {
    blocks.push({ kind: 'report', title: '诊断报告', data: normalizeReport(structured.report) })
  }
  if (structured.suggestedRepair && typeof structured.suggestedRepair === 'object') {
    blocks.push({ kind: 'repairSuggestion', title: '修复建议', data: structured.suggestedRepair })
  }
  // 排障手册引用：search_runbooks 的 Data 是 {query, snippets}，
  // 每个片段带标题和关键句，渲染成“参考了哪篇手册”，
  // 不再只显示 ev-1、ev-2 这种编号。runbooks 不是为某个案例写的特例，
  // 是 RAG 知识库：加一篇 .md 重启就进索引，模型自己搜。
  const rbData = structured.runbooks as { snippets?: unknown } | undefined
  if (rbData && Array.isArray(rbData.snippets) && rbData.snippets.length > 0) {
    blocks.push({ kind: 'runbooks', title: '参考手册', data: rbData.snippets as RunbookSnippet[] })
  }
  return blocks
}
