// 前后端共用的类型定义，和 Go 后端返回的 JSON 字段一一对应。
// 后端改了字段名这里也要跟着改，不要各写各的。

// 通用 Patch 里的一条操作（Slice 9）：op 只支持 replace，
// path 是白名单里的字段路径，value 是新值。
export interface PatchOperation {
  op: string
  path: string
  value: unknown
}

// 修复建议（Slice 9，通用 Patch 格式）：
// patches 是要改的字段列表，不再只认内存。
// field / oldValue / newValue 是旧字段，保留做兼容，
// 老的 memory-only 卡读进来时后端已转成 patches，
// 前端两个都给了时以 patches 为准。
export interface SuggestedRepair {
  namespace: string
  deployment: string
  container: string
  patches?: PatchOperation[]
  field?: string
  oldValue?: string
  newValue?: string
  reason: string
}

// 创建建议（Slice 10）：和修复建议卡平级，只是动作从“改”换成“创建”。
// 字段和后端 store.CreationSpec 的 JSON 一一对应。
export interface SuggestedCreation {
  namespace: string
  name: string
  image: string
  replicas: number
  container?: string
  memoryLimit?: string
  memoryRequest?: string
  cpuLimit?: string
  cpuRequest?: string
  reason?: string
}

// 转换时前端传给后端的目标和建议（Slice 10）：
// 建议卡上的转换按钮把这张卡的目标（namespace/kind/name）和
// Patches（或创建规格）一起传过去，后端优先用它们，
// 不再去历史里猜目标——以前只传会话 id，猜到的是最早的 demo-api。
export interface ConvertPayload {
  namespace?: string
  kind?: string
  name?: string
  container?: string
  question?: string
  patches?: PatchOperation[]
  creation?: SuggestedCreation
}

export interface Report {
  facts: string[] | null
  cause: string
  evidenceIds: string[] | null
  recommendations: string[] | null
  openQuestions: string[] | null
  confidence: string
  suggestedRepair?: SuggestedRepair | null
  suggestedCreation?: SuggestedCreation | null
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
  // 通用 Patch 列表（Slice 9）：审批卡按这个列表展示，
  // 老提案没有它时按 oldValue/newValue 显示一条内存变更。
  patches?: PatchOperation[]
  // 提案类型（Slice 10）："patch" 是改已有 Deployment 的字段，
  // "create" 是创建一个新的 Deployment。老提案没有它时按 "patch" 处理。
  kind?: string
  // 创建提案的完整规格（Slice 10），只有 kind="create" 时才有。
  creation?: SuggestedCreation | null
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

// Token 用量（token 用量统计）：一次或累计若干次模型调用的用量，
// 值直接取模型响应里的 usage 对象。三个值都为 0 表示还没产生用量
//（或响应里没带 usage），前端这时不显示用量行，不编数字。
export interface Usage {
  promptTokens: number
  completionTokens: number
  totalTokens: number
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
  usage?: Usage
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
  suggestedCreation?: SuggestedCreation | null
}

export interface ConversationMessage {
  id: number
  conversationId: string
  role: 'user' | 'assistant' | 'tool'
  content: string
  toolCalls?: string
  structured?: string
  // 同一轮的关联 ID（v0.12.0）：同一轮的用户 / tool / 助手消息共享一个
  // round_ 开头的 ID；工具调用只存在 tool 消息里，助手消息不再抄一份
  // tool_calls。老数据没有它，读出来是 undefined，走位置推导回退。
  roundId?: string
  createdAt: string
}

export interface Conversation {
  id: string
  title: string
  createdAt: string
  updatedAt: string
}

// 配置中心（Slice 8）：和后端 internal/config.Config 的 JSON 字段一一对应。
// 注意：这里没有 MODEL_API_KEY——Key 只从环境变量读，
// 绝不进配置文件、绝不在 GET /api/config 里返回、绝不在前端显示。
export interface AppConfig {
  chat: { maxSteps: number; maxHistoryMessages: number; summary: { enabled: boolean; triggerMessages: number } }
  run: { maxSteps: number; timeoutSeconds: number; maxToolCallsPerRound: number }
  model: { timeoutSeconds: number; temperature: number; maxTokens: number }
  repair: { maxMemoryMi: number; maxRatio: number }
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

/** 取一条助手消息对应的本轮工具调用（v0.12.0，真关联）：
 * 有 roundId 时数同 roundId 的 tool 消息——工具调用只存在 tool 消息里，
 * 每调一次一条，助手消息不再抄一份，数出来的就是实际调过的次数。
 * 老数据没有 roundId 时回退到位置推导：上一条用户消息到本条助手消息
 * 之间的 tool 消息；再没有就读助手消息里存的老 tool_calls 副本。
 * 这样新老会话的“查了 N 个工具”都对得上，活动面板也不会再出现两遍。 */
export function toolCallsForAssistant(
  msgs: ConversationMessage[],
  assistant: ConversationMessage,
): ToolCallRecord[] {
  if (assistant.roundId) {
    const out: ToolCallRecord[] = []
    for (const m of msgs) {
      if (m.role === 'tool' && m.roundId === assistant.roundId) out.push(...parseToolCalls(m))
    }
    return out
  }
  const idx = msgs.findIndex((m) => m.id === assistant.id)
  const calls: ToolCallRecord[] = []
  for (let i = idx - 1; i >= 0; i--) {
    const m = msgs[i]
    if (m.role === 'user' || m.role === 'assistant') break
    if (m.role === 'tool') calls.unshift(...parseToolCalls(m))
  }
  if (calls.length > 0) return calls
  return parseToolCalls(assistant)
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
  | 'creationSuggestion'
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
  // 每个元素必须是带字符串 name 的对象：以前这里只认数组，
  // list_workload_pods 的名字字符串数组也通过了，
  // 前端拿字符串当 PodRow 渲，r.name、r.namespace 全是 undefined，
  // 标题栏个数对、行里全空。现在形状不对直接不认，
  // 宁可不渲染这块，也不要再渲染一排空单元格。
  for (const p of o.pods) {
    if (!p || typeof p !== 'object' || typeof (p as PodRow).name !== 'string') return null
  }
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
    suggestedCreation: r.suggestedCreation ?? null,
  }
}

/** 把后端 Structured 的 key 映射成带类型的结果块列表。 */
export function buildBlocks(structured: Record<string, unknown> | null | undefined): ResultBlock[] {
  const blocks: ResultBlock[] = []
  if (!structured || typeof structured !== 'object') return blocks
  if (structured.overview && typeof structured.overview === 'object') {
    blocks.push({ kind: 'overview', title: '集群总览', data: structured.overview })
  }
  // Pod 表格有三个来源，各走各的 key、标题各不相同：
  // problemPods 是有问题的 Pod，pods 是 list_pods 列的全部 Pod，
  // workloadPods 是某 Deployment 下的 Pod。以前 problemPods 和 pods
  // 撞在一个 ?? 里，而 pods 那份一度是名字字符串数组，
  // asPods 只认数组不认元素形状，渲出来就是一排空单元格。
  // 现在三个来源都渲染（同轮调了两个就出两张表），
  // 形状不对的 asPods 直接不认，宁可不渲染也不渲空行。
  let podBlockPushed = false
  const pushPodList = (data: unknown, title: string, label: string) => {
    const rows = asPods(data)
    if (rows) {
      blocks.push({ kind: 'podList', title, data: { pods: rows, label } })
      podBlockPushed = true
    }
  }
  pushPodList(structured.problemPods, '有问题的 Pod', '有问题 Pod')
  const podsData = structured.pods as { namespace?: unknown } | null | undefined
  const podsNs =
    typeof podsData?.namespace === 'string' && podsData.namespace ? podsData.namespace : ''
  pushPodList(
    structured.pods,
    podsNs ? `${podsNs} 的 Pod` : 'Pod 列表',
    podsNs ? `${podsNs} 的 Pod` : 'Pod',
  )
  const wpData = structured.workloadPods as { deployment?: unknown } | null | undefined
  const wpDep =
    typeof wpData?.deployment === 'string' && wpData.deployment ? wpData.deployment : ''
  pushPodList(
    structured.workloadPods,
    wpDep ? `${wpDep} 的 Pod` : 'Deployment 的 Pod',
    wpDep ? `${wpDep} 的 Pod` : 'Deployment 的 Pod',
  )
  if (!podBlockPushed && structured.pod && typeof structured.pod === 'object') {
    // 单个 Pod（桩模型深挖路径）：包成一行表格，不再单独写一套展示。
    blocks.push({ kind: 'podList', title: 'Pod', data: { pods: [structured.pod as PodRow], label: 'Pod' } })
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
  // 创建建议（Slice 10）：和修复建议卡平级，只是动作从“改”换成“创建”。
  if (structured.suggestedCreation && typeof structured.suggestedCreation === 'object') {
    blocks.push({ kind: 'creationSuggestion', title: '创建建议', data: structured.suggestedCreation })
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
