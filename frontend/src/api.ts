// API 层：只负责发请求和处理 HTTP 错误，不做业务逻辑。
// 所有接口都是同源的 /api/*，开发模式下 Vite 会代理到 Go 服务。

import type {
  AppConfig,
  ChatReply,
  Conversation,
  ConversationMessage,
  ConvertPayload,
  RunSnapshot,
  RunStep,
} from './types'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const body = (await res.json().catch(() => ({}))) as Record<string, unknown> & T
  if (!res.ok) {
    const msg = typeof body.error === 'string' ? body.error : `请求失败（HTTP ${res.status}）`
    throw new Error(msg)
  }
  return body as T
}

export interface ConversationListResponse {
  conversations: Conversation[]
}

export interface ConversationDetailResponse {
  id: string
  title: string
  messages: ConversationMessage[]
}

export interface EventsResponse {
  runId: string
  after: number
  events: RunStep[]
}

export interface DecisionResponse {
  approvalId: string
  runId: string
  status: string
  approver: string
  alreadyDecided: boolean
  note: string
}

export async function createConversation(): Promise<Conversation> {
  return request<Conversation>('/api/conversations', { method: 'POST', body: '{}' })
}

export async function listConversations(): Promise<Conversation[]> {
  const r = await request<ConversationListResponse>('/api/conversations')
  return r.conversations ?? []
}

export async function getConversation(id: string): Promise<ConversationDetailResponse> {
  return request<ConversationDetailResponse>(`/api/conversations/${id}`)
}

export async function postChatMessage(conversationId: string, content: string): Promise<ChatReply> {
  return request<ChatReply>(`/api/conversations/${conversationId}/messages`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

// 转换时把建议卡的目标和建议一起传过去（Slice 10）：
// 修复建议卡传 namespace/kind/name/container/patches，
// 创建建议卡传 namespace/kind/name/creation，后端优先用它们，
// 不再去历史里猜目标。什么都不传时后端回退到历史猜测（老客户端兼容）。
export async function convertConversation(
  conversationId: string,
  body?: ConvertPayload,
): Promise<{ runId: string; conversationId: string }> {
  return request<{ runId: string; conversationId: string }>(
    `/api/conversations/${conversationId}/convert`,
    { method: 'POST', body: JSON.stringify(body ?? {}) },
  )
}

export async function createRun(input: {
  namespace: string
  kind: string
  name: string
  question: string
}): Promise<{ runId: string; status: string }> {
  return request<{ runId: string; status: string }>('/api/runs', {
    method: 'POST',
    body: JSON.stringify(input),
  })
}

export async function getRun(runId: string): Promise<RunSnapshot> {
  return request<RunSnapshot>(`/api/runs/${runId}`)
}

export async function listRunEvents(runId: string, after: number): Promise<RunStep[]> {
  const r = await request<EventsResponse>(`/api/runs/${runId}/events?after=${after}`)
  return r.events ?? []
}

export async function decideApproval(
  runId: string,
  approvalId: string,
  decision: 'approved' | 'rejected',
): Promise<DecisionResponse> {
  return request<DecisionResponse>(`/api/runs/${runId}/approvals/${approvalId}/decision`, {
    method: 'POST',
    body: JSON.stringify({ decision, approver: 'varo' }),
  })
}

// 配置中心（Slice 8）：GET 读当前配置，POST 保存并热加载。
// 返回里没有 MODEL_API_KEY——Key 只从环境变量读，前端拿不到它、也改不了它。
export async function getConfig(): Promise<AppConfig> {
  return request<AppConfig>('/api/config')
}

export async function updateConfig(cfg: AppConfig): Promise<{ config: AppConfig; note: string }> {
  return request<{ config: AppConfig; note: string }>('/api/config', {
    method: 'POST',
    body: JSON.stringify(cfg),
  })
}
