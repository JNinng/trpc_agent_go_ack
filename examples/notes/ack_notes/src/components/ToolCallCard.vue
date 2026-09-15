<script setup lang="ts">
import type { ChatItem } from '@/stores/chat'

defineProps<{ item: Extract<ChatItem, { kind: 'tool' }> }>()

/** 把工具参数字符串格式化为缩进 JSON，解析失败时原样返回。 */
function prettyArgs(args: string): string {
  try {
    return JSON.stringify(JSON.parse(args), null, 2)
  } catch {
    return args
  }
}

/** 把工具结果字符串格式化为缩进 JSON，解析失败时原样返回。 */
function prettyResult(result?: string): string {
  if (!result) return ''
  try {
    return JSON.stringify(JSON.parse(result), null, 2)
  } catch {
    return result
  }
}
</script>

<template>
  <div class="tool-card">
    <div class="tool-head">
      <span class="tool-icon">🛠</span>
      <span class="tool-name">{{ item.name }}</span>
      <span class="tool-state">{{ item.result ? '已执行' : '调用中…' }}</span>
    </div>
    <pre v-if="item.argsText" class="tool-block">{{ prettyArgs(item.argsText) }}</pre>
    <pre v-if="item.result" class="tool-block tool-result">{{ prettyResult(item.result) }}</pre>
  </div>
</template>

<style scoped>
.tool-card {
  max-width: 560px;
  border: 1px solid #d9dde3;
  border-radius: 10px;
  padding: 10px 12px;
  background: #fff;
  font-size: 13px;
}
.tool-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.tool-name {
  font-weight: 600;
}
.tool-state {
  margin-left: auto;
  font-size: 12px;
  color: #8a919c;
}
.tool-block {
  margin: 8px 0 0;
  padding: 8px;
  background: #f7f8fa;
  border-radius: 6px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 240px;
  overflow: auto;
  font-size: 12px;
}
.tool-result {
  background: #f0f9eb;
}
</style>
