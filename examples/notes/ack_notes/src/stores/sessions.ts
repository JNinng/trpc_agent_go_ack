/** 会话列表：新建、切换、删除、标题展示。 */

import { defineStore } from 'pinia'
import { deleteSession, listSessions, newId } from '@/api/client'
import type { SessionItem } from '@/api/types'

export const useSessionsStore = defineStore('sessions', {
  state: () => ({
    items: [] as SessionItem[],
    loading: false,
    error: '',
    /** 本地刚生成、尚未在服务端创建的 threadId（首次发消息前不拉历史）。 */
    pendingNewId: '',
  }),
  actions: {
    /** 从服务端拉取会话列表。 */
    async refresh() {
      this.loading = true
      try {
        this.items = await listSessions()
        this.error = ''
      } catch (e) {
        this.error = String(e)
      } finally {
        this.loading = false
      }
    },
    /** 生成本地新会话 ID 并标记 pendingNewId（首次发消息后才在服务端落库）。 */
    newThreadId(): string {
      this.pendingNewId = newId()
      return this.pendingNewId
    },
    /** 删除会话并刷新列表。 */
    async remove(id: string) {
      await deleteSession(id)
      await this.refresh()
    },
  },
})
