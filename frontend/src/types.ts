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
  facts: string[]
  cause: string
  evidenceIds: string[]
  recommendations: string[]
  openQuestions: string[]
  confidence: string
  suggestedRepair?: SuggestedRepair | null
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
