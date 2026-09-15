<script setup lang="ts">
import { onBeforeUnmount, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useChatStore } from '@/stores/chat'
import { useSessionsStore } from '@/stores/sessions'
import ChatInput from '@/components/ChatInput.vue'
import ConfirmationCard from '@/components/ConfirmationCard.vue'
import ToolCallCard from '@/components/ToolCallCard.vue'

const route = useRoute()
const chat = useChatStore()
const sessions = useSessionsStore()

/** 依据路由参数打开会话；本地新建、尚未落库的会话跳过历史加载。 */
async function openFromRoute() {
  const threadId = String(route.params.threadId ?? '')
  if (threadId && threadId !== chat.threadId) {
    // 本地刚新建的会话在服务端尚不存在，跳过历史加载，首条消息发送时创建。
    const isNew = sessions.pendingNewId === threadId
    if (isNew) sessions.pendingNewId = ''
    await chat.open(threadId, { skipHistory: isNew })
  }
}

watch(() => route.params.threadId, openFromRoute, { immediate: true })

// 一轮运行结束后刷新侧栏（标题/时间）。
watch(
  () => chat.streaming,
  (now, before) => {
    if (before && !now) sessions.refresh()
  },
)

onBeforeUnmount(() => chat.closeStreams())
</script>

<template>
  <section class="chat-view">
    <div class="messages" id="messages">
      <div v-if="!chat.threadId || (chat.items.length === 0 && !chat.connected)" class="welcome">
        <h2>Agent 笔记助手 · 工具许可演示</h2>
        <p>工具按许可层级分类：read_note 直接执行，write_note / delete_note 需要你确认，drop_all_notes 被禁用。</p>
        <p>试试"读取 n1"或"删除 n2"：确认卡片上可勾选"记住授权"，之后同工具免确认。</p>
      </div>
      <template v-for="item in chat.items" :key="item.id">
        <div v-if="item.kind === 'text'" class="row" :class="item.role">
          <div class="bubble" :class="item.role">
            {{ item.text }}<span v-if="item.streaming" class="cursor">▍</span>
          </div>
        </div>
        <div v-else-if="item.kind === 'tool'" class="row assistant">
          <ToolCallCard :item="item" />
        </div>
        <div v-else-if="item.kind === 'confirm'" class="row assistant">
          <ConfirmationCard :item="item" />
        </div>
      </template>
      <p v-if="chat.error" class="stream-error">{{ chat.error }}</p>
    </div>
    <ChatInput />
  </section>
</template>

<style scoped>
.chat-view {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.messages {
  flex: 1;
  overflow-y: auto;
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.welcome {
  margin: auto;
  text-align: center;
  color: #5b6270;
}
.row {
  display: flex;
}
.row.user {
  justify-content: flex-end;
}
.bubble {
  max-width: 72%;
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 14px;
  white-space: pre-wrap;
  word-break: break-word;
}
.bubble.user {
  background: #2f6fed;
  color: #fff;
  border-bottom-right-radius: 4px;
}
.bubble.assistant {
  background: #f2f3f5;
  border-bottom-left-radius: 4px;
}
.cursor {
  animation: blink 1s infinite;
}
.stream-error {
  color: #d4380d;
  font-size: 12px;
  text-align: center;
}
@keyframes blink {
  50% {
    opacity: 0;
  }
}
</style>
