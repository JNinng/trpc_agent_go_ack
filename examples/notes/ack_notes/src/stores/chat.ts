/** 聊天状态：把 AG-UI 事件流（实时 + 快照 + follow）归并为渲染项列表。 */

import { defineStore } from 'pinia'
import {
  decideConfirmation,
  loadHistory,
  newId,
  runAgent,
  listPending,
} from '@/api/client'
import {
  CONFIRMATION_EVENT_NAME,
  type AguiEvent,
  type ConfirmationPayload,
  type ConfirmationStatus,
  type RememberScope,
  type SnapshotMessage,
} from '@/api/types'

/** 恢复路径决定后重开历史流前的等待：让续跑运行先落库，
 * 全新快照直接包含结果（规避上游 follow 流时间戳缺陷，见设计 D4）。 */
const RECOVERY_REOPEN_DELAY_MS = 200

/** 聊天渲染项，三种互斥形态：
 *  - text：用户/助手文本气泡（streaming 表示仍在流式接收）；
 *  - tool：工具调用卡片（argsText 增量拼接，result 到达后补齐）；
 *  - confirm：工具确认卡片（payload.status 驱动展示状态）。 */
export type ChatItem =
  | { kind: 'text'; id: string; role: 'user' | 'assistant'; text: string; streaming?: boolean }
  | { kind: 'tool'; id: string; toolCallId: string; name: string; argsText: string; result?: string }
  | { kind: 'confirm'; id: string; payload: ConfirmationPayload }

/** chat store 的状态形状。 */
interface ChatState {
  /** 当前会话 ID（空串表示尚未打开任何会话）。 */
  threadId: string
  /** 渲染项列表（按到达顺序）。 */
  items: ChatItem[]
  /** 一轮运行进行中（用于禁用发送按钮）。 */
  streaming: boolean
  /** 历史/follow 流连接中。 */
  connected: boolean
  /** 最近一次请求错误文案。 */
  error: string
  /** confirmationId → 决定请求进行中（防止重复提交）。 */
  deciding: Record<string, boolean>
}

/** 当前聊天流与历史流的中止器（模块级，同一时刻各最多一个）。 */
let abortChat: AbortController | null = null
let abortHistory: AbortController | null = null

export const useChatStore = defineStore('chat', {
  state: (): ChatState => ({
    threadId: '',
    items: [],
    streaming: false,
    connected: false,
    error: '',
    deciding: {},
  }),
  getters: {
    /** 仍处于等待确认状态的卡片列表。 */
    pendingConfirms(state): ChatItem[] {
      return state.items.filter((i) => i.kind === 'confirm' && i.payload.status === 'pending')
    },
  },
  actions: {
    /** 切换会话：重置状态并加载历史（快照 + follow 续读 + 孤儿 pending 合并）。 */
    async open(threadId: string, opts: { skipHistory?: boolean } = {}) {
      this.closeStreams()
      this.$patch({ threadId, items: [], streaming: false, connected: false, error: '' })
      if (!threadId || opts.skipHistory) return

      abortHistory = new AbortController()
      const controller = abortHistory
      this.connected = true
      try {
        await loadHistory(threadId, (ev) => this.applyEvent(ev), controller.signal)
      } catch (e) {
        if (!isAbort(e)) this.error = String(e)
      } finally {
        if (abortHistory === controller) abortHistory = null
        this.connected = false
      }
      // 快照/follow 结束后兜底合并服务端视角的待确认（覆盖卡片丢失等边角）。
      if (!controller.signal.aborted && this.threadId === threadId) {
        try {
          const pendings = await listPending(threadId)
          for (const p of pendings) this.upsertConfirmation({ ...p, status: 'pending' })
        } catch {
          /* 非致命 */
        }
      }
    },

    /** 发送用户消息并流式接收本轮运行事件。 */
    async send(text: string) {
      const trimmed = text.trim()
      if (!trimmed || this.streaming) return
      if (!this.threadId) return

      this.items.push({ kind: 'text', id: newId(), role: 'user', text: trimmed })
      this.streaming = true
      this.error = ''

      abortChat = new AbortController()
      const controller = abortChat
      try {
        await runAgent(this.threadId, trimmed, (ev) => this.applyEvent(ev), controller.signal)
      } catch (e) {
        if (!isAbort(e)) this.error = String(e)
      } finally {
        if (abortChat === controller) abortChat = null
        this.streaming = false
      }
    },

    /** 用户点击 同意/拒绝。同意可携带 remember 生成许可（之后同工具免确认）。
     * live/recovery 由后端决定：live 结果经原流回来；recovery 在短暂等待后
     * 重开历史流，让快照直接包含续跑结果。 */
    async decide(confirmationId: string, approved: boolean, remember?: RememberScope) {
      if (this.deciding[confirmationId]) return
      this.deciding[confirmationId] = true
      try {
        const { path } = await decideConfirmation(this.threadId, confirmationId, approved, remember)
        // 乐观更新；若运行已死（recovery），重开后的快照会校正。
        this.setConfirmationStatus(confirmationId, approved ? 'approved' : 'denied')
        if (path === 'recovery' && this.threadId) {
          const tid = this.threadId
          setTimeout(() => {
            if (this.threadId === tid) void this.open(tid)
          }, RECOVERY_REOPEN_DELAY_MS)
        }
      } catch (e) {
        this.error = String(e)
      } finally {
        delete this.deciding[confirmationId]
      }
    },

    /** 中断当前所有 SSE 流（切换会话、组件卸载时调用）。 */
    closeStreams() {
      abortChat?.abort()
      abortChat = null
      abortHistory?.abort()
      abortHistory = null
    },

    /** AG-UI 事件归并器。 */
    applyEvent(ev: AguiEvent) {
      switch (ev.type) {
        case 'TEXT_MESSAGE_START':
          this.upsertText(ev.messageId ?? newId(), '', true)
          break
        case 'TEXT_MESSAGE_CONTENT':
          this.appendText(ev.messageId ?? '', ev.delta ?? '')
          break
        case 'TEXT_MESSAGE_END': {
          const item = this.items.find((i) => i.kind === 'text' && i.id === ev.messageId)
          if (item && item.kind === 'text') item.streaming = false
          break
        }
        case 'TOOL_CALL_START':
          this.upsertTool(ev.toolCallId ?? newId(), ev.toolCallName ?? ev.toolName ?? '')
          break
        case 'TOOL_CALL_ARGS':
          this.appendToolArgs(ev.toolCallId ?? '', ev.delta ?? '')
          break
        case 'TOOL_CALL_END':
          break
        case 'TOOL_CALL_RESULT':
          this.setToolResult(ev.toolCallId ?? '', ev.content ?? '')
          // 结果到达即意味着该调用的确认已终结（覆盖 recovery 孤儿路径
          // 没有状态事件的场景）。
          this.resolveConfirmationByToolCall(ev.toolCallId ?? '')
          break
        case 'CUSTOM':
          if (ev.name === CONFIRMATION_EVENT_NAME && ev.value) {
            this.upsertConfirmation(ev.value)
          }
          break
        case 'MESSAGES_SNAPSHOT':
          if (ev.messages) this.loadSnapshot(ev.messages)
          break
        case 'RUN_ERROR':
          // 会话尚不存在（新建未发消息/直达不存在会话的 URL）：按空会话处理。
          if ((ev.message ?? '').includes('session not found')) break
          this.items.push({
            kind: 'text',
            id: newId(),
            role: 'assistant',
            text: `运行出错：${ev.message ?? '未知错误'}`,
          })
          break
        default:
          break
      }
    },

    /** MESSAGES_SNAPSHOT：整表重建（历史回放 / 刷新恢复）。 */
    loadSnapshot(messages: SnapshotMessage[]) {
      const items: ChatItem[] = []
      for (const m of messages) {
        if (m.role === 'user') {
          items.push({ kind: 'text', id: m.id, role: 'user', text: asText(m.content) })
          continue
        }
        if (m.role === 'assistant') {
          const text = asText(m.content)
          if (text) items.push({ kind: 'text', id: m.id, role: 'assistant', text })
          for (const tc of m.toolCalls ?? []) {
            items.push({
              kind: 'tool',
              id: tc.id,
              toolCallId: tc.id,
              name: tc.function.name,
              argsText: tc.function.arguments,
            })
          }
          continue
        }
        if (m.role === 'tool') {
          // 工具结果消息：挂到对应工具卡片（按插入序应已存在）。
          const target = [...items].reverse().find((i) => i.kind === 'tool' && i.toolCallId === m.toolCallId)
          if (target && target.kind === 'tool') {
            target.result = asText(m.content)
          } else {
            items.push({
              kind: 'tool',
              id: m.id,
              toolCallId: m.toolCallId ?? m.id,
              name: m.name ?? '',
              argsText: '',
              result: asText(m.content),
            })
          }
          continue
        }
        if (m.role === 'activity' && isConfirmationActivity(m)) {
          const value = confirmationValueOf(m)
          if (value) items.push({ kind: 'confirm', id: value.confirmationId, payload: value })
        }
      }
      // 同一确认的 pending/终态活动消息会成对出现：只保留最新状态。
      this.items = dedupeConfirmations(items)
    },

    /** 新建或重置一条助手文本项（TEXT_MESSAGE_START 用）。 */
    upsertText(id: string, text: string, streaming: boolean) {
      const found = this.items.find((i) => i.kind === 'text' && i.id === id)
      if (found && found.kind === 'text') {
        found.text = text
        found.streaming = streaming
        return
      }
      this.items.push({ kind: 'text', id, role: 'assistant', text, streaming })
    },

    /** 向已有文本项追加增量（TEXT_MESSAGE_CONTENT 用）。 */
    appendText(id: string, delta: string) {
      const found = this.items.find((i) => i.kind === 'text' && i.id === id)
      if (found && found.kind === 'text') found.text += delta
    },

    /** 新建工具卡片；同 toolCallId 幂等（TOOL_CALL_START 用）。 */
    upsertTool(toolCallId: string, name: string) {
      if (this.items.some((i) => i.kind === 'tool' && i.toolCallId === toolCallId)) return
      this.items.push({ kind: 'tool', id: toolCallId, toolCallId, name, argsText: '' })
    },

    /** 向工具卡片追加参数增量（TOOL_CALL_ARGS 用）。 */
    appendToolArgs(toolCallId: string, delta: string) {
      const found = this.items.find((i) => i.kind === 'tool' && i.toolCallId === toolCallId)
      if (found && found.kind === 'tool') found.argsText += delta
    },

    /** 记录工具执行结果（TOOL_CALL_RESULT 用）。 */
    setToolResult(toolCallId: string, result: string) {
      const found = this.items.find((i) => i.kind === 'tool' && i.toolCallId === toolCallId)
      if (found && found.kind === 'tool') found.result = result
    },

    /** 新建或更新确认卡片（CUSTOM 事件 / 待确认查询合并用）。 */
    upsertConfirmation(p: ConfirmationPayload) {
      const found = this.items.find((i) => i.kind === 'confirm' && i.id === p.confirmationId)
      if (found && found.kind === 'confirm') {
        // 状态只能前进：已决（approved/denied）不被迟到的 pending 事件回退。
        if (found.payload.status === 'pending' || p.status !== 'pending') found.payload = { ...p }
        return
      }
      this.items.push({ kind: 'confirm', id: p.confirmationId, payload: { ...p } })
    },

    /** 直接设置确认状态（提交决定后的本地乐观更新用）。 */
    setConfirmationStatus(confirmationId: string, status: ConfirmationStatus) {
      const found = this.items.find((i) => i.kind === 'confirm' && i.id === confirmationId)
      if (found && found.kind === 'confirm') found.payload.status = status
    },

    /** 工具结果到达时按 toolCallId 兜底终结对应的待确认卡片。 */
    resolveConfirmationByToolCall(toolCallId: string) {
      const found = this.items.find((i) => i.kind === 'confirm' && i.payload.toolCallId === toolCallId)
      if (found && found.kind === 'confirm' && found.payload.status === 'pending') {
        // 无法从结果本身区分同意/拒绝时默认标 approved；随后到达的状态事件会校正。
        found.payload.status = 'approved'
      }
    },
  },
})

/** 把消息 content 规整为字符串（对象序列化为 JSON）。 */
function asText(content: unknown): string {
  if (typeof content === 'string') return content
  if (content == null) return ''
  try {
    return JSON.stringify(content)
  } catch {
    return String(content)
  }
}

/** 快照中 CustomEvent 被降级为 role=activity 的消息（content 内嵌 name/value）。 */
function isConfirmationActivity(m: SnapshotMessage): boolean {
  if (m.activityType !== 'CUSTOM' && m.activityType !== undefined) return false
  const c = m.content as { name?: string } | undefined
  return !!c && typeof c === 'object' && c.name === CONFIRMATION_EVENT_NAME
}

/** 从 activity 消息中取出内嵌的确认负载。 */
function confirmationValueOf(m: SnapshotMessage): ConfirmationPayload | null {
  const c = m.content as { value?: ConfirmationPayload } | undefined
  return c?.value ?? null
}

/** 同一确认多次出现时只保留状态最新的一个（保持其余项原有顺序）。 */
function dedupeConfirmations(items: ChatItem[]): ChatItem[] {
  const latest = new Map<string, Extract<ChatItem, { kind: 'confirm' }>>()
  for (const item of items) {
    if (item.kind !== 'confirm') continue
    const prev = latest.get(item.id)
    if (!prev || statusRank(item.payload.status) >= statusRank(prev.payload.status)) {
      latest.set(item.id, item)
    }
  }
  const out: ChatItem[] = []
  const takenConfirmIds = new Set<string>()
  for (const item of items) {
    if (item.kind !== 'confirm') {
      out.push(item)
      continue
    }
    const winner = latest.get(item.id)
    if (winner === item && !takenConfirmIds.has(item.id)) {
      out.push(winner)
      takenConfirmIds.add(item.id)
    }
  }
  return out
}

/** 状态排序权重：已决（1）≥ 待确认（0）。 */
function statusRank(s: ConfirmationStatus): number {
  return s === 'pending' ? 0 : 1
}

/** 判断错误是否为主动中止引发（切换会话等场景不算错误）。 */
function isAbort(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError'
}
