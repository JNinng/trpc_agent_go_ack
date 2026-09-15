<script setup lang="ts">
import { ref } from 'vue'
import { useChatStore } from '@/stores/chat'
import type { ChatItem } from '@/stores/chat'
import type { RememberScope } from '@/api/types'

const props = defineProps<{ item: Extract<ChatItem, { kind: 'confirm' }> }>()
const chat = useChatStore()

// 同意时可勾选"记住授权"并选择作用域；拒绝不产生许可。
const remember = ref(false)
const scope = ref<RememberScope>('session')

/** 把确认卡片的工具参数格式化为缩进 JSON，解析失败时原样展示。 */
function prettyArgs(): string {
  const a = props.item.payload.arguments
  if (!a) return ''
  const text = typeof a === 'string' ? a : JSON.stringify(a)
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

/** 提交同意/拒绝决定给 chat store；同意且勾选时携带 remember。 */
function decide(approved: boolean) {
  chat.decide(props.item.payload.confirmationId, approved, approved && remember.value ? scope.value : undefined)
}
</script>

<template>
  <div class="confirm-card" :data-status="item.payload.status">
    <div class="confirm-head">
      <span class="confirm-icon">{{ item.payload.status === 'pending' ? '❓' : item.payload.status === 'approved' ? '✅' : '⛔' }}</span>
      <span class="confirm-title">工具执行确认</span>
      <span class="confirm-state">
        {{
          item.payload.status === 'pending'
            ? '等待确认'
            : item.payload.status === 'approved'
              ? '已同意'
              : '已拒绝'
        }}
      </span>
    </div>
    <p class="confirm-desc">
      Agent 请求执行工具 <code>{{ item.payload.toolName }}</code>，需要你确认后才会真正执行：
    </p>
    <pre v-if="prettyArgs()" class="confirm-args">{{ prettyArgs() }}</pre>
    <template v-if="item.payload.status === 'pending'">
      <label class="remember-row">
        <input v-model="remember" type="checkbox" />
        记住授权（之后免确认）
        <select v-if="remember" v-model="scope" class="remember-scope">
          <option value="session">本会话</option>
          <option value="global">全局（我的全部会话）</option>
        </select>
      </label>
      <div class="confirm-actions">
        <button class="btn-approve" :disabled="!!chat.deciding[item.payload.confirmationId]" @click="decide(true)">
          同意执行
        </button>
        <button class="btn-deny" :disabled="!!chat.deciding[item.payload.confirmationId]" @click="decide(false)">
          拒绝
        </button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.confirm-card {
  max-width: 560px;
  border: 1px solid #f0c36d;
  border-radius: 10px;
  padding: 10px 12px;
  background: #fffbe6;
  font-size: 13px;
}
.confirm-card[data-status='approved'] {
  border-color: #b7eb8f;
  background: #f6ffed;
}
.confirm-card[data-status='denied'] {
  border-color: #ffa39e;
  background: #fff2f0;
}
.confirm-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.confirm-title {
  font-weight: 600;
}
.confirm-state {
  margin-left: auto;
  font-size: 12px;
  color: #8a919c;
}
.confirm-desc {
  margin: 6px 0;
}
.confirm-args {
  margin: 6px 0;
  padding: 8px;
  background: #f7f8fa;
  border-radius: 6px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 200px;
  overflow: auto;
  font-size: 12px;
}
.remember-row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 6px 0;
  font-size: 12px;
  color: #5b6270;
  flex-wrap: wrap;
}
.remember-scope {
  padding: 2px 4px;
  border: 1px solid #d9dde3;
  border-radius: 4px;
  font-size: 12px;
}
.confirm-actions {
  display: flex;
  gap: 8px;
}
.btn-approve,
.btn-deny {
  padding: 6px 16px;
  border: none;
  border-radius: 6px;
  font-size: 13px;
  cursor: pointer;
}
.btn-approve {
  background: #2f6fed;
  color: #fff;
}
.btn-approve:hover {
  background: #2558c4;
}
.btn-deny {
  background: #fff;
  color: #d4380d;
  border: 1px solid #ffa39e;
}
.btn-deny:hover {
  background: #fff2f0;
}
.btn-approve:disabled,
.btn-deny:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}
</style>
