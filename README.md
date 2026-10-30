# ack — trpc-agent-go 的工具调用许可库

`ack` 是构建在 [trpc-agent-go](https://github.com/trpc-group/trpc-agent-go) AG-UI 协议栈之上的 Go 公共库，为 Agent 应用提供**工具调用许可**（tool permission）能力：

- **层级分类**：工具按 `allow`（默认放行）/ `confirm`（需人工确认）/ `deny`（禁用）分级；
- **策略源**：外部裁决系统（如风控）在每次调用时介入，可强制确认、背书降级或直接拒绝；
- **许可**：用户预先授予的授权（会话级/全局级）自动满足确认要求，决定时勾选"记住"即可生成；
- **人工确认全生命周期**：运行存活时阻塞唤醒（live 路径），服务重启后孤儿确认经会话历史软恢复续跑（recovery 路径）；
- **授权终态观察**：全部自动放行/拒绝终态经回调外报宿主审计，人工决定归宿主 Decide 路由；
- **HTTP 绑定**：确认决定/查询路由可挂载到任意 `http.ServeMux`。

完整设计（评估管线、事件契约、不变量、部署约束）见 [docs/module_design.md](./docs/module_design.md)。

## 仓库布局

```
ack.go / permission.go / policy.go / confirm.go / recovery.go / grants.go / payload.go / errors.go / doc.go
  核心库（包 ack）
ackhttp/   确认决定/查询的 HTTP 路由（仅依赖标准库与 ack）
acktest/   测试夹具：脚本化模型、假续跑 runner
examples/notes/   端到端示例：笔记助手（Go 后端 + Vue3 前端 ack_notes/）
docs/module_design.md   设计文档（唯一设计事实来源）
```

## 快速开始（示例）

```bash
# 1. 构建前端（产物不入库，仓库内有占位页）
cd examples/notes/ack_notes
pnpm install && pnpm build

# 2. 运行示例（离线 mock 模型，内存会话）
cd ../../..
go run ./examples/notes
# 打开 http://127.0.0.1:8080

# 以 MCP 工具模式运行：自动启动 MCP server 并对接 agent
#（启动日志打印 MCP 端点，外部 MCP 客户端可直连观察）
go run ./examples/notes -mcp
```

默认脚本演示完整许可闭环：读取 n1（allow 直行）→ 删除 n2（确认卡片）→ 删除 n3（勾选"记住授权"后免确认）→ 总结。

### 示例环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `NOTES_ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `NOTES_SESSION_BACKEND` | `memory` | `memory` / `postgres`（重启恢复演示需 postgres） |
| `NOTES_PG_DSN` | 本机默认 | PostgreSQL 连接串（库不存在时自动创建） |
| `NOTES_LLM` | `deepseek` | `mock`（脚本化模型，离线）或真实提供方 |
| `NOTES_API_KEY` | 空 | 为空自动回退 mock；也读 `DEEPSEEK_API_KEY` |
| `NOTES_BASE_URL` / `NOTES_MODEL` | DeepSeek | OpenAI 兼容接口与模型名 |
| `NOTES_RISK_POLICY` | `0` | 置 `1` 启用 mock 风控策略源（演示强制确认/背书/deny 三形态） |

命令标记：`-mcp` 启用 MCP 工具模式——自动启动 MCP server（streamable HTTP）并把同一批笔记工具原样接入 agent，验证 ack 对 MCP 工具同管线生效；默认 function 模式（进程内直接调用）。

### 示例端点

| 路由 | 说明 |
| --- | --- |
| `/` | 前端（`pnpm build` 后嵌入单二进制；开发用 `pnpm dev` 代理） |
| `POST /agui` / `POST /agui/history` / `POST /agui/cancel` | AG-UI SSE |
| `POST /ack/confirmation` | 确认决定（`{threadId, confirmationId, approved, remember?}`，返回 `{path: live\|recovery}`） |
| `GET /ack/confirmations?threadId=` | 待确认查询（含孤儿，`live=false`） |
| `GET /api/sessions` / `DELETE /api/sessions/{id}` | 会话列表/删除（示例级参考实现） |

## 库接入

```go
gw := ack.New(svc,
    ack.WithApp("biz"),                    // 须与 AG-UI runner 的 AppName 一致
    ack.WithUser("u1"),
    ack.WithTiers(map[string]ack.Tier{"delete_note": ack.TierConfirm}),
    ack.WithPolicies(riskControl),         // 可选：策略源链
    ack.WithGrants(ack.MemoryGrants()),    // 业务可换自有存储
    ack.WithStaticTools(readNote, deleteNote),
    ack.WithPermissionObserver(func(ctx context.Context, e ack.PermissionEvent) {
        log.Println("permission settled", e.Call.ToolName, e.Basis) // 可选：终态外报
    }),
)

r := aguirunner.New(coreRunner,
    aguirunner.WithAppName("biz"),
    aguirunner.WithSessionService(svc),
    aguirunner.WithRunOptionResolver(gw.RunOptionResolver()),
    aguirunner.WithTimeout(10*time.Minute), // 未决确认最终由超时结算为拒绝
)
_ = gw.Bind(r)

ackhttp.Mount(mux, gw) // POST /ack/confirmation、GET /ack/confirmations
```

前端约定：消费 `tool_confirmation` 自定义事件渲染确认卡片，决定时调用 `POST /ack/confirmation`（可携带 `remember`），重连时以待确认查询补偿孤儿状态，续跑结果经 `/agui/history` follow 流读取。事件与存储契约见设计文档 §8。

## 测试

```bash
go test -race -count=1 ./...          # 库 + ackhttp + 示例集成测试
cd examples/notes/ack_notes && pnpm exec vitest run   # 前端单测
```

Go 集成测试对恢复路径的断言采用"快照轮询"编写，不依赖 history follow 流的自然终止（见设计文档 D4 对上游时间戳缺陷的说明）。

## 许可

[MIT](./LICENSE)
