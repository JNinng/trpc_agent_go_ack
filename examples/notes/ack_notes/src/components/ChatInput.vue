<script setup lang="ts">
import { ref } from 'vue'
import { useChatStore } from '@/stores/chat'

const chat = useChatStore()
const text = ref('')

/** 先清空输入再发送（回车或点击按钮触发；真正发送由 store 把关）。 */
async function submit() {
  const value = text.value
  text.value = ''
  await chat.send(value)
}
</script>

<template>
  <div class="chat-input">
    <textarea
      v-model="text"
      :disabled="!chat.threadId"
      rows="2"
      :placeholder="chat.threadId ? '输入消息，回车发送（Shift+回车换行）' : '点击左侧「新建会话」开始'"
      @keydown.enter.exact.prevent="submit"
    />
    <button class="send" :disabled="!text.trim() || chat.streaming || !chat.threadId" @click="submit">
      {{ chat.streaming ? '运行中…' : '发送' }}
    </button>
  </div>
</template>

<style scoped>
.chat-input {
  display: flex;
  gap: 8px;
  padding: 12px 16px;
  border-top: 1px solid #e3e5e8;
  background: #fff;
}
textarea {
  flex: 1;
  resize: none;
  padding: 10px;
  border: 1px solid #d9dde3;
  border-radius: 8px;
  font-size: 14px;
  font-family: inherit;
}
.send {
  padding: 0 20px;
  border: none;
  border-radius: 8px;
  background: #2f6fed;
  color: #fff;
  font-size: 14px;
  cursor: pointer;
}
.send:disabled {
  background: #a8c1f5;
  cursor: not-allowed;
}
</style>
