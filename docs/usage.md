# ack 使用文档

本指南说明如何在业务中接入与使用 ack。文档导航：

| 文档 | 内容 |
| --- | --- |
| [usage.md](./usage.md)（本文） | 接入步骤、配置项、HTTP API、前端对接、测试 |
| [mechanism.md](./mechanism.md) | 实现视角的原理机制（评估管线、live/recovery 路径、并发不变量） |
| [module_design.md](./module_design.md) | 完整设计契约（事件、存储、不变量、部署约束），设计事实来源 |
| [README](../README.md) | 项目概览与快速开始 |

## 1. 安装

```bash
go get github.com/jninng/trpc_agent_go_ack
```

根包名为 `ack`，要求 Go ≥ 1.26.2（见 go.mod），依赖 [trpc-agent-go](https://github.com/trpc-group/trpc-agent-go) 的 AG-UI 协议栈。

## 2. 最小接入

三个装配动作：构造网关 → 接入 AG-UI runner → 挂载 HTTP 路由。

```go
package main

import (
    "net/http"
    "time"

    agui "trpc.group/trpc-go/trpc-agent-go/server/agui"
    aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
    runner "trpc.group/trpc-go/trpc-agent-go/runner"
    "trpc.group/trpc-go/trpc-agent-go/session/inmemory"

    "github.com/jninng/trpc_agent_go_ack"
    "github.com/jninng/trpc_agent_go_ack/ackhttp"
)

func main() {
    svc := inmemory.NewSessionService() // 会话存储由业务选择并持有生命周期
    core := runner.NewRunner("biz", ag, runner.WithSessionService(svc)) // ag 为业务 Agent

    gw := ack.New(svc,
        ack.WithApp("biz"),                              // 须与 runner 的 AppName 一致
        ack.WithUser("u1"),                              // 固定身份；多用户用 WithUserFunc
        ack.WithTiers(map[string]ack.Tier{
            "delete_note": ack.TierConfirm,              // 需人工确认
            "drop_all":    ack.TierDeny,                 // 禁用
        }),                                              // 未分级工具默认放行
        ack.WithGrants(ack.MemoryGrants()),              // 许可源；缺省则 Remember 被忽略
        ack.WithStaticTools(readNote, deleteNote),       // 恢复路径代执行所需
    )

    aguiRunner := aguirunner.New(core,
        aguirunner.WithAppName("biz"),
        aguirunner.WithSessionService(svc),
        aguirunner.WithRunOptionResolver(gw.RunOptionResolver()), // 每次运行挂许可策略
        aguirunner.WithMessagesSnapshotFollowEnabled(true),        // /history follow 流（recovery 读取）
        aguirunner.WithTimeout(10*time.Minute),          // 未决确认最终由超时结算为拒绝
    )
    _ = gw.Bind(aguiRunner) // 启用恢复路径；未 Bind 时 Decide 孤儿返回 ErrNotBound

    server, _ := agui.New(core,
        agui.WithAppName("biz"),
        agui.WithSessionService(svc),
        agui.WithPath("/"),
        agui.WithMessagesSnapshotEnabled(true),
        agui.WithMessagesSnapshotPath("/history"),
        agui.WithCancelEnabled(true),
        agui.WithRunnerFactory(func(runner.Runner, ...aguirunner.Option) (aguirunner.Runner, error) {
            return aguiRunner, nil // 用挂好许可策略的 runner 替换默认构造
        }),
    )

    mux := http.NewServeMux()
    mux.Handle("/agui/", http.StripPrefix("/agui", server.Handler()))
    ackhttp.Mount(mux, gw)                               // POST /ack/confirmation、GET /ack/confirmations
    _ = mux
}
```

接入后行为：未分级工具直接放行；`delete_note` 每次调用发出 `tool_confirmation` 事件等待人工决定；用户勾选"记住"后同类调用免确认；服务重启后未决确认仍可决定并续跑（需持久化会话存储等前置条件，见 §8）。各路径原理见 [mechanism.md](./mechanism.md)。

直接使用 core runner（不经 aguirunner）的业务，改用 `gw.PolicyFor(sessionID)` 取得 `tool.PermissionPolicyFunc` 自行挂载。

## 3. 配置项参考

全部 `Option` 见 [ack.go](../ack.go)。

| 配置 | 说明 | 缺省 |
| --- | --- | --- |
| `WithApp(name)` | 应用名，会话存储键的组成部分，**必须与 runner AppName 一致** | `"ack"` |
| `WithUser(id)` / `WithUserFunc(fn)` | 用户身份；动态版从请求 ctx 取鉴权结果，优先于静态版 | `"user"` |
| `WithDefaultTier(t)` | 未分级工具的层级 | `TierAllow` |
| `WithTiers(m)` | 按工具名静态分级 | 空 |
| `WithTierFunc(fn)` | 动态分级（可按入参分类），优先于静态表 | nil |
| `WithNamedPolicies(ps...)` | 具名策略源，名字进入终态事件 `Source` 字段 | 空 |
| `WithPolicies(src...)` | 匿名策略源；与具名合成一条咨询链（具名在前、匿名在后） | 空 |
| `WithGrants(gs)` | 许可源 | nil（许可永不命中，Remember 忽略） |
| `WithTools(res)` / `WithStaticTools(ts...)` | 工具解析器，恢复路径代执行用；缺省时同意的孤儿确认降级为拒绝 | nil |
| `WithPermissionObserver(ob)` | 授权终态观察者，供宿主审计 | nil |
| `WithDenyText(fn)` | 人工拒绝文案；须自行保留"不得重试"指令 | 默认文案 |
| `WithLogger(l)` | 结构化日志器 | `slog.Default()` |
| `gw.Bind(r)` | 注入续跑 runner，启用恢复路径 | 未 Bind → `ErrNotBound` |

## 4. 分级、策略源与许可

- **层级**（`Tier`）：`allow` 默认放行 / `confirm` 需人工确认（可被背书或 Grant 满足）/ `deny` 禁用（不可放行）。
- **策略源**：实现 `PolicySource` 接口，每次工具调用被同步咨询，位于关键路径应低延迟：

  ```go
  type PolicySource interface {
      Consult(ctx context.Context, call ack.ToolCall) (ack.Verdict, error)
  }
  ```

  返回 `VerdictAbstain`（不参与）/ `VerdictAllow`（背书，confirm 降级放行）/ `VerdictConfirm`（强制确认，Grant 不可满足）/ `VerdictDeny`（绝对拒绝）。**咨询出错按 confirm 处理（fail-safe）**；需要 fail-open 时在实现内吞错返回 abstain。判定次序与依据词表见 [mechanism.md](./mechanism.md#二许可评估管线)。
- **许可源**：实现 `GrantSource` 接口（`Lookup` 须会话级先于全局级命中 / `Save`）。`ack.MemoryGrants()` 为进程内实现，重启即失；生产环境接入自有存储（如用户设置系统）。

## 5. 授权终态审计

```go
ack.WithPermissionObserver(func(ctx context.Context, e ack.PermissionEvent) {
    log.Println(e.Call.ToolName, e.Allowed, e.Basis, e.Source)
})
```

- 全部**自动**放行/拒绝终态回调（依据 `Basis`：tier / verdict / endorsement / grant / duplicate / canceled / persist-failed）；**人工决定不通知**，归宿主 Decide 路由审计——每次工具调用恰好一条终态，二者互斥且共同覆盖。
- 回调在结算同步路径执行，应快速返回，重负载自行异步化；ctx 保证未取消，可安全执行 IO。

## 6. HTTP API

由 `ackhttp.Mount(mux, gw, ackhttp.WithPrefix("/ack"))` 挂载（见 [ackhttp/http.go](../ackhttp/http.go)），仅依赖标准库。

| 路由 | 说明 |
| --- | --- |
| `POST {p}/confirmation` | 提交决定。请求 `{threadId, confirmationId, approved, remember?}`（remember ∈ `""`/`"session"`/`"global"`）；返回 `{path: "live"\|"recovery"}`。错误映射：`ErrNotFound` → 404（不存在/已决定/并发后到者），`ErrRunActive` → 409（续跑时会话仍有运行） |
| `GET {p}/confirmations?threadId=` | 待确认查询，合并存活等待者与持久化记录；孤儿项 `live=false` |

身份从请求 ctx 解析（鉴权中间件写入）；未配置 `WithUserFunc` 时用 `WithUser` 静态值。

## 7. 前端对接约定

1. **渲染卡片**：消费 AG-UI 自定义事件 `tool_confirmation`（`Payload` 形状见 [payload.go](../payload.go)），按 `status=pending` 渲染。
2. **提交决定**：调 `POST /ack/confirmation`；需要"记住授权"时携带 `remember`。
3. **重连补偿**：重连后先调 `GET /ack/confirmations?threadId=` 补偿孤儿状态（`live=false` 的项同样可决定）。
4. **读取续跑结果**：recovery 路径的事件经 `/agui/history` follow 流读取。
5. **终态渲染**：`status=approved/denied` 时更新卡片为终态。

事件与存储契约细节见 [module_design.md](./module_design.md)。

## 8. 恢复路径（recovery）启用要求

服务重启后孤儿确认可决定续跑，需同时满足：

- `gw.Bind(r)` 注入 AG-UI runner（`aguirunner.New` 返回值直接满足 `ResumeRunner`）；
- 会话存储使用**持久化后端**（如 `session/postgres`），内存后端重启即失，无从恢复；
- 配置 `WithTools`/`WithStaticTools`，否则同意的孤儿确认降级为"工具不可用"拒绝（安全优先）。

机制细节见 [mechanism.md](./mechanism.md#3-恢复路径recovery)。

## 9. 测试

```bash
go test -race -count=1 ./...   # 库 + ackhttp + 示例集成测试
```

业务侧确认流集成测试可复用 [acktest](../acktest/) 夹具（无真实 LLM 的确定性测试）：

- `acktest.NewScriptedModel(name, steps...)`：脚本化模型，按轮次回放文本/工具调用，脚本耗尽后重复最后一步；
- `acktest.NewResumeRecorder()`：假续跑 runner，记录 `Run` 调用参数，`FailWith` 可注入错误演练失败路径。

## 10. 端到端示例

[examples/notes](../examples/notes/) 是笔记助手示例（Go 后端 + Vue3 前端），演示完整许可闭环：读取（allow 直行）→ 删除（确认卡片）→ 勾选"记住"（免确认）→ 总结。运行方式与环境变量（会话后端切换、mock 风控策略源、MCP 工具模式等）见 [README](../README.md)。
