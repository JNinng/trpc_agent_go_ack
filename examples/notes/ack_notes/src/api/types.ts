/** AG-UI 协议事件与业务类型的宽松定义（字段与后端/SDK 输出对齐）。 */

/** AG-UI 事件类型。 */
export type AguiEventType =
  | 'RUN_STARTED'
  | 'RUN_FINISHED'
  | 'RUN_ERROR'
  | 'TEXT_MESSAGE_START'
  | 'TEXT_MESSAGE_CONTENT'
  | 'TEXT_MESSAGE_END'
  | 'TOOL_CALL_START'
  | 'TOOL_CALL_ARGS'
  | 'TOOL_CALL_END'
  | 'TOOL_CALL_RESULT'
  | 'MESSAGES_SNAPSHOT'
  | 'CUSTOM'

/** 一次 AG-UI 事件；不同事件类型只用到各自分组的字段。 */
export interface AguiEvent {
  type: AguiEventType
  timestamp?: number
  // RUN_* 事件：运行标识
  runId?: string
  threadId?: string
  // TEXT_MESSAGE_* 事件：文本流式增量
  messageId?: string
  delta?: string
  // TOOL_CALL_* 事件：工具调用流式增量与结果
  toolCallId?: string
  toolName?: string
  toolCallName?: string
  parentMessageId?: string
  content?: string
  // CUSTOM 事件：自定义事件名与负载
  name?: string
  value?: ConfirmationPayload
  // MESSAGES_SNAPSHOT 事件：整表消息快照
  messages?: SnapshotMessage[]
  // RUN_ERROR 事件：错误信息
  message?: string
  [key: string]: unknown
}

/** 快照/历史中的消息（角色与字段按后端实际输出宽松定义）。 */
export interface SnapshotMessage {
  id: string
  role: 'user' | 'assistant' | 'tool' | 'activity' | string
  content?: unknown
  name?: string
  toolCallId?: string
  toolCalls?: SnapshotToolCall[]
  /** activity 消息的原始事件类型（如 CUSTOM）。 */
  activityType?: string
}

/** 快照中助手消息携带的工具调用。 */
export interface SnapshotToolCall {
  id: string
  type: string
  function: { name: string; arguments: string }
}

/** 确认状态：待确认 / 已同意 / 已拒绝。 */
export type ConfirmationStatus = 'pending' | 'approved' | 'denied'

/** 许可作用域（决定携带 remember 时使用）。 */
export type RememberScope = 'session' | 'global'

/** 工具确认卡片数据（与后端 ack.Payload 对齐）。 */
export interface ConfirmationPayload {
  confirmationId: string
  toolCallId: string
  toolName: string
  /** 工具入参：实时事件里是对象，快照里可能是 JSON 字符串。 */
  arguments?: Record<string, unknown> | string
  sessionId: string
  status: ConfirmationStatus
  decidedBy?: string
  createdAt: number
}

/** 待确认查询结果；live=false 表示原运行已丢失（重启后的孤儿）。 */
export interface PendingConfirmation extends ConfirmationPayload {
  live: boolean
}

/** 决定接口的返回：实际生效的路径。 */
export type DecidePath = 'live' | 'recovery'

/** 会话列表条目。 */
export interface SessionItem {
  id: string
  title: string
  createdAt: string
  updatedAt: string
}

/** 确认卡片使用的自定义事件名（与后端 ack.EventName 保持一致）。 */
export const CONFIRMATION_EVENT_NAME = 'tool_confirmation'
