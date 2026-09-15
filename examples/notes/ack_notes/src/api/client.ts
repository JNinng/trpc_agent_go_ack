/** HTTP/SSE 客户端：聊天流、历史回放、确认决定、会话管理。 */

import { logSseReceived } from './debug'
import type { AguiEvent, DecidePath, PendingConfirmation, RememberScope, SessionItem } from './types'

/** POST 一个 SSE 请求，把每个 data 事件回调给 onEvent；resolve 于流结束。 */
export async function ssePost(
  path: string,
  body: unknown,
  onEvent: (ev: AguiEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  const resp = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
    body: JSON.stringify(body),
    signal,
  })
  if (!resp.ok || !resp.body) {
    const text = await resp.text().catch(() => '')
    throw new Error(`SSE ${path} 请求失败（${resp.status}）${text}`)
  }
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let sep: number
    // SSE 事件以空行分隔；逐块解析完整的 data 负载。
    while ((sep = buffer.indexOf('\n\n')) >= 0) {
      const block = buffer.slice(0, sep)
      buffer = buffer.slice(sep + 2)
      const data = block
        .split('\n')
        .filter((l) => l.startsWith('data:'))
        .map((l) => l.replace(/^data:\s?/, ''))
        .join('')
      if (!data) continue
      try {
        const ev = JSON.parse(data) as AguiEvent
        logSseReceived(path, ev)
        onEvent(ev)
      } catch {
        // 忽略非 JSON 负载（如心跳）
      }
    }
  }
}

/** 生成一个 UUID（消息 ID / 运行 ID / 本地会话 ID 通用）。 */
export function newId(): string {
  return crypto.randomUUID()
}

/** 构造 AG-UI 运行请求体；不携带用户消息时仅用于历史回放。 */
function chatInput(threadId: string, userText?: string) {
  const messages = userText ? [{ id: newId(), role: 'user', content: userText }] : []
  return { threadId, runId: newId(), messages }
}

/** 发起一轮对话；事件经 onEvent 流回。 */
export function runAgent(
  threadId: string,
  userText: string,
  onEvent: (ev: AguiEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  // 尾斜杠避免外层 mux 对 /agui 的 307 重定向。
  return ssePost('/agui/', chatInput(threadId, userText), onEvent, signal)
}

/** 加载历史快照；服务端会在快照后自动 follow 续读未结束的运行。 */
export function loadHistory(
  threadId: string,
  onEvent: (ev: AguiEvent) => void,
  signal?: AbortSignal,
): Promise<void> {
  return ssePost('/agui/history', chatInput(threadId), onEvent, signal)
}

/** 用户对某个待确认工具调用做出决定；同意时可携带 remember 生成许可。
 * 返回实际生效的路径：live（唤醒阻塞运行）或 recovery（重启后续跑）。 */
export async function decideConfirmation(
  threadId: string,
  confirmationId: string,
  approved: boolean,
  remember?: RememberScope,
): Promise<{ path: DecidePath }> {
  const resp = await fetch('/ack/confirmation', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ threadId, confirmationId, approved, remember: remember ?? '' }),
  })
  if (!resp.ok) {
    const text = await resp.text().catch(() => '')
    throw new Error(`确认请求失败（${resp.status}）：${text}`)
  }
  return resp.json()
}

/** 查询会话当前待确认项（含重启后的孤儿 pending）。 */
export async function listPending(threadId: string): Promise<PendingConfirmation[]> {
  const resp = await fetch(`/ack/confirmations?threadId=${encodeURIComponent(threadId)}`)
  if (!resp.ok) throw new Error(`查询待确认失败（${resp.status}）`)
  return resp.json()
}

/** 拉取全部会话列表。 */
export async function listSessions(): Promise<SessionItem[]> {
  const resp = await fetch('/api/sessions')
  if (!resp.ok) throw new Error(`查询会话列表失败（${resp.status}）`)
  return resp.json()
}

/** 删除指定会话及其全部记录。 */
export async function deleteSession(id: string): Promise<void> {
  const resp = await fetch(`/api/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
  if (!resp.ok && resp.status !== 204) throw new Error(`删除会话失败（${resp.status}）`)
}
