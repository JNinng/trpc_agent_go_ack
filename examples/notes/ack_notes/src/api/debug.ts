/**
 * SSE 调试输出：开发模式（pnpm dev）默认开启，便于对照协议排查事件流；
 * 生产构建默认静默，需要时在控制台执行
 * `localStorage.setItem('ack-notes:sse-debug', '1')` 后刷新即可开启。
 */

/** 是否输出 SSE 调试日志。 */
export const sseDebugEnabled = import.meta.env.DEV || debugFlagFromStorage()

/** 打印一条收到的 SSE 事件：前缀含流路径与事件类型，便于在控制台过滤。 */
export function logSseReceived(path: string, event: { type?: string }) {
  if (!sseDebugEnabled) return
  console.log(`[sse recv] ${path} ${event.type ?? 'unknown'}`, event)
}

/** 读取本地调试开关；存储不可用（如隐私模式）时按未开启处理。 */
function debugFlagFromStorage(): boolean {
  try {
    return localStorage.getItem('ack-notes:sse-debug') === '1'
  } catch {
    return false
  }
}
