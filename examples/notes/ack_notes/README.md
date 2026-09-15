# ack notes — 示例前端

`ack` 库端到端示例（`examples/notes`）的 Vue3 前端：会话侧栏、聊天流、工具调用卡片、确认卡片（含"记住授权"）。

## 命令

```bash
pnpm install
pnpm dev        # 开发模式（/ack、/api、/agui 代理到 127.0.0.1:8080）
pnpm build      # 构建到 dist/（Go 侧 go:embed 嵌入单二进制）
pnpm test:unit  # Vitest 单测（chat reducer 归并逻辑）
```

构建产物不入库：`pnpm build` 会覆盖仓库内的占位 `dist/index.html`，提交前用 `git checkout -- dist/index.html` 还原占位页。

## 与后端的契约

- 确认卡片：消费 AG-UI 自定义事件 `tool_confirmation`，按 `confirmationId` 配对 pending → 终态；
- 决定提交：`POST /ack/confirmation`，同意可携带 `remember`（`session`/`global`）；
- 孤儿补偿：重连/刷新后经 `GET /ack/confirmations` 合并服务端待确认项；
- 恢复路径：决定返回 `path=recovery` 时，短暂延时后重开 `/agui/history`，以全新快照读取续跑结果。

完整契约见仓库 [docs/module_design.md](../../../docs/module_design.md) §8。
