/** chat store 归并逻辑的单测：错误处理、确认卡片生命周期、快照回放、决定提交。 */

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const { decideMock, loadHistoryMock } = vi.hoisted(() => ({
  decideMock: vi.fn(),
  loadHistoryMock: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  decideConfirmation: decideMock,
  loadHistory: loadHistoryMock,
  listPending: vi.fn(async () => []),
  newId: () => 'id-generated',
  runAgent: vi.fn(),
}))

import { useChatStore } from '../chat'
import { CONFIRMATION_EVENT_NAME, type AguiEvent, type ConfirmationPayload } from '@/api/types'

/** 构造一个确认负载，除状态外字段固定。 */
function confirmValue(status: ConfirmationPayload['status']): ConfirmationPayload {
  return {
    confirmationId: 'c1',
    toolCallId: 't1',
    toolName: 'delete_note',
    arguments: { note_id: 'n2' },
    sessionId: 's1',
    status,
    createdAt: 1,
  }
}

describe('chat store reducer', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    decideMock.mockReset()
    loadHistoryMock.mockReset()
    loadHistoryMock.mockResolvedValue(undefined)
  })

  it('新建未落库会话的 session not found 错误按空会话处理', () => {
    const chat = useChatStore()
    chat.applyEvent({ type: 'RUN_STARTED', runId: 'h1' } as AguiEvent)
    chat.applyEvent({
      type: 'RUN_ERROR',
      message: 'load history: get track events: session not found',
    } as AguiEvent)
    expect(chat.items).toHaveLength(0)
    expect(chat.error).toBe('')
  })

  it('其他 RUN_ERROR 仍渲染为错误消息', () => {
    const chat = useChatStore()
    chat.applyEvent({ type: 'RUN_ERROR', message: 'boom' } as AguiEvent)
    expect(chat.items).toHaveLength(1)
    expect(chat.items[0]).toMatchObject({ kind: 'text', role: 'assistant' })
  })

  it('确认卡片生命周期：pending → approved，且状态不回退', () => {
    const chat = useChatStore()
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    expect(chat.pendingConfirms).toHaveLength(1)

    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('approved'),
    } as AguiEvent)
    expect(chat.pendingConfirms).toHaveLength(0)

    // 迟到的 pending 事件不应把已决状态改回等待确认
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    expect(chat.pendingConfirms).toHaveLength(0)
  })

  it('TOOL_CALL_RESULT 兜底终结待确认卡片（recovery 孤儿路径）', () => {
    const chat = useChatStore()
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    chat.applyEvent({
      type: 'TOOL_CALL_RESULT',
      toolCallId: 't1',
      content: '{"deleted":true}',
    } as AguiEvent)
    expect(chat.pendingConfirms).toHaveLength(0)
  })

  it('快照回放：工具调用/结果/确认卡片完整还原且 pending+approved 去重', () => {
    const chat = useChatStore()
    chat.applyEvent({
      type: 'MESSAGES_SNAPSHOT',
      messages: [
        { id: 'm1', role: 'user', content: '删除 n2' },
        {
          id: 'a1',
          role: 'assistant',
          content: '',
          toolCalls: [{ id: 't1', type: 'function', function: { name: 'delete_note', arguments: '{"note_id":"n2"}' } }],
        },
        {
          id: 'act1',
          role: 'activity',
          activityType: 'CUSTOM',
          content: { name: CONFIRMATION_EVENT_NAME, value: confirmValue('pending') },
        },
        {
          id: 'act2',
          role: 'activity',
          activityType: 'CUSTOM',
          content: { name: CONFIRMATION_EVENT_NAME, value: confirmValue('approved') },
        },
        { id: 'tool1', role: 'tool', toolCallId: 't1', content: '{"deleted":true}' },
        { id: 'a2', role: 'assistant', content: '操作已完成。' },
      ],
    } as AguiEvent)

    const kinds = chat.items.map((i) => i.kind)
    expect(kinds).toEqual(['text', 'tool', 'confirm', 'text'])
    const tool = chat.items.find((i) => i.kind === 'tool')
    expect(tool).toMatchObject({ result: '{"deleted":true}' })
    const confirm = chat.items.find((i) => i.kind === 'confirm')
    expect(confirm).toMatchObject({ payload: { status: 'approved' } })
  })

  it('decide 携带 remember 提交，live 路径乐观更新状态', async () => {
    const chat = useChatStore()
    chat.threadId = 's1'
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    decideMock.mockResolvedValue({ path: 'live' })

    await chat.decide('c1', true, 'session')

    expect(decideMock).toHaveBeenCalledWith('s1', 'c1', true, 'session')
    expect(chat.items.find((i) => i.kind === 'confirm')).toMatchObject({
      payload: { status: 'approved' },
    })
    expect(chat.pendingConfirms).toHaveLength(0)
  })

  it('recovery 路径在延迟后重开历史流读取续跑结果', async () => {
    vi.useFakeTimers()
    const chat = useChatStore()
    chat.threadId = 's1'
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    decideMock.mockResolvedValue({ path: 'recovery' })

    await chat.decide('c1', true)
    expect(loadHistoryMock).not.toHaveBeenCalled()

    vi.advanceTimersByTime(300)
    expect(loadHistoryMock).toHaveBeenCalledWith('s1', expect.any(Function), expect.anything())
    vi.useRealTimers()
  })

  it('decide 失败呈现错误且不更新状态', async () => {
    const chat = useChatStore()
    chat.threadId = 's1'
    chat.applyEvent({
      type: 'CUSTOM',
      name: CONFIRMATION_EVENT_NAME,
      value: confirmValue('pending'),
    } as AguiEvent)
    decideMock.mockRejectedValue(new Error('确认请求失败（404）'))

    await chat.decide('c1', true)
    expect(chat.error).toContain('404')
    expect(chat.pendingConfirms).toHaveLength(1)
  })
})
