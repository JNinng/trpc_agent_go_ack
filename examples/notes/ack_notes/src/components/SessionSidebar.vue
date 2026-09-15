<script setup lang="ts">
import { onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { useSessionsStore } from '@/stores/sessions'

const router = useRouter()
const sessions = useSessionsStore()

onMounted(() => sessions.refresh())

/** 新建会话：生成本地 threadId 并跳转到对应聊天路由。 */
function createSession() {
  router.push({ name: 'chat', params: { threadId: sessions.newThreadId() } })
}

/** 删除会话（二次确认）；删除的是当前打开的会话时回到空白页。 */
async function remove(id: string) {
  if (!window.confirm('删除该会话及其全部记录？')) return
  await sessions.remove(id)
  if (router.currentRoute.value.params.threadId === id) {
    router.push({ name: 'chat', params: { threadId: '' } })
  }
}
</script>

<template>
  <aside class="sidebar">
    <button class="new-chat" @click="createSession">＋ 新建会话</button>
    <p v-if="sessions.error" class="sidebar-error">{{ sessions.error }}</p>
    <ul class="session-list">
      <li v-for="s in sessions.items" :key="s.id" class="session-item">
        <RouterLink
          :to="{ name: 'chat', params: { threadId: s.id } }"
          class="session-link"
          :title="s.title"
        >
          <span class="session-title">{{ s.title }}</span>
          <span class="session-time">{{ s.updatedAt }}</span>
        </RouterLink>
        <button class="session-delete" title="删除会话" @click.stop="remove(s.id)">✕</button>
      </li>
    </ul>
    <p v-if="!sessions.loading && sessions.items.length === 0" class="sidebar-empty">
      暂无历史会话
    </p>
  </aside>
</template>

<style scoped>
.sidebar {
  width: 260px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px 12px;
  background: #f5f6f8;
  border-right: 1px solid #e3e5e8;
  overflow-y: auto;
}
.new-chat {
  padding: 10px;
  border: none;
  border-radius: 8px;
  background: #2f6fed;
  color: #fff;
  font-size: 14px;
  cursor: pointer;
}
.new-chat:hover {
  background: #2558c4;
}
.session-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.session-item {
  display: flex;
  align-items: center;
  gap: 4px;
}
.session-link {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px;
  border-radius: 8px;
  color: inherit;
  text-decoration: none;
}
.session-link.router-link-active {
  background: #e6ecff;
}
.session-link:hover {
  background: #eceef1;
}
.session-title {
  font-size: 13px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.session-time {
  font-size: 11px;
  color: #8a919c;
}
.session-delete {
  border: none;
  background: none;
  color: #8a919c;
  cursor: pointer;
  padding: 4px;
  border-radius: 4px;
  visibility: hidden;
}
.session-item:hover .session-delete {
  visibility: visible;
}
.session-delete:hover {
  color: #d4380d;
  background: #fdecea;
}
.sidebar-empty {
  font-size: 12px;
  color: #8a919c;
  text-align: center;
}
.sidebar-error {
  font-size: 12px;
  color: #d4380d;
}
</style>
