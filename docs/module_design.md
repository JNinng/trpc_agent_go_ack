# ack 模块设计

> 文档导航：接入与配置见 [usage.md](./usage.md)，实现原理与机制图解见 [mechanism.md](./mechanism.md)。本文是设计事实来源，二者与其不一致时以本文为准。

## 1. 定位

`ack` 是构建在 [trpc-agent-go](https://github.com/trpc-group/trpc-agent-go) AG-UI 协议栈之上的 Go 公共库，为 Agent 应用提供**工具调用许可**（tool permission）能力：工具按层级分类——默认允许、需人工确认、禁用；外部策略源（如风控）可在每次调用时介入裁决；用户预先授予的许可（会话级、全局级）可自动满足确认要求；仅当无任何放行依据时才阻塞等待人工决定。

库对外提供三件事：

1. **许可网关**（核心）：评估每次工具调用的处置（放行/确认/拒绝），并承载人工确认的完整生命周期；自动授权终态经观察回调外报宿主（§7.5）。
2. **HTTP 绑定**：可挂载到任意 `http.ServeMux` 的确认决定/查询路由。
3. **测试支撑与示例**：无真实 LLM 时的确定性测试夹具、端到端示例工程。

模块名 `ack`，导入路径 `github.com/jninng/trpc_agent_go_ack`（下文示例省略导入路径）。

## 2. 范围

**内含**

- 许可评估：层级配置、策略源链、许可查询与合成规则。
- 人工确认的完整生命周期：请求 → 等待 → 决定（同意/拒绝）→ 结算；仅评估结果为「确认」的调用进入此流程。
- 运行存活时的阻塞唤醒，与服务重启后的孤儿确认恢复。
- 确认事件随 AG-UI track 持久化（历史回放可重现）。
- 待确认指针写入 `Session.State`（重启后可恢复）。
- 人工决定可选生成许可（记住授权），经许可源接口写入。
- 授权终态观察：全部自动放行/拒绝终态经 `PermissionObserver` 外报宿主，人工决定不通知（§7.5）。
- 确认决定与待确认查询的标准库 HTTP 路由。

**范围外**

- 策略源与许可存储的业务实现：风控系统、用户设置服务由业务提供，库只定义接口。
- 许可管理（列出/吊销）的 REST 与界面：业务对自有存储实现。
- 参数级许可模式（按入参细粒度放行）：可经分级函数表达，库不内置。
- 会话列表/删除的 REST 暴露：`session.Service` 已提供该能力，标题派生等属应用逻辑，由业务自建（示例工程中给出参考实现）。
- 多实例部署下的确认路由转发：确认决定与运行须同进程，横向扩展需外围保证会话粘性。
- 待确认项的过期清理（TTL）：未决确认保持到被决定或会话删除。
- 鉴权与用户体系：身份以注入方式提供，认证由业务中间件完成。
- 通用审批中台：库绑定 AG-UI 协议栈，不抽象独立于 Agent 运行的审批流。

## 3. 术语

| 术语 | 定义 |
| --- | --- |
| 许可网关（gateway） | 库的核心对象：评估工具调用处置、登记等待者、结算决定。 |
| 层级（tier） | 工具的基线许可属性：`allow`（默认放行）/ `confirm`（需人工确认）/ `deny`（禁用）。 |
| 判定（verdict） | 策略源对一次工具调用返回的意见：`abstain`（无立场）/ `allow` / `confirm` / `deny`。 |
| 策略源（policy source） | 外部裁决系统（如风控）在库中的接入点，每次工具调用被同步咨询。 |
| 许可（grant） | 用户对某工具的预先授权，分会话级与全局级（该用户的全部会话）；可满足 confirm 层级的确认要求。 |
| 背书（endorsement） | 策略源的 `allow` 判定；可将 confirm 层级降级为放行。 |
| 强制确认 | 由策略源 `confirm` 判定触发的人工确认；不可被许可满足。 |
| 确认（confirmation） | 一次工具调用的人工审批请求，确认 ID 即工具调用 ID。 |
| 直接路径（live） | 发出确认的运行仍存活时，决定唤醒阻塞的运行继续执行。 |
| 恢复路径（recovery） | 运行已不存在（如服务重启）时，由网关代执行或合成拒绝结果，并拉起续跑运行。 |
| 孤儿（orphan） | 运行已不存在、仍处于 pending 的确认。 |
| 待确认指针 | 持久化在 `Session.State` 中的 pending 确认表，是恢复路径的判定依据。 |
| 等待者（waiter） | 网关内存中阻塞等待决定的确认项。 |

## 4. 许可模型

### 4.1 评估管线

每次工具调用（内置 function 工具与 MCP 工具同管线）在框架权限策略挂点处进入评估，按固定步骤得出处置：

| 步 | 条件 | 处置 | 后续满足方式 |
| --- | --- | --- | --- |
| 1 | 层级 `deny`，或任一判定为 `deny` | 拒绝执行，返回 denied 结果 | 无，不可降级 |
| 2 | 任一判定为 `confirm` | 进入人工确认（强制确认） | 仅人工决定 |
| 3 | 层级 `allow` | 放行执行 | — |
| 4 | 层级 `confirm` 且任一判定为 `allow` | 放行执行（背书） | — |
| 5 | 命中许可（会话级优先于全局级） | 放行执行 | — |
| 6 | 其余（层级 `confirm` 且无背书无许可） | 进入人工确认 | 人工决定；决定可携带 Remember 生成许可 |

评估示例矩阵：

| 层级 | 判定 | 许可 | 处置 |
| --- | --- | --- | --- |
| allow | — | —（不查询） | 放行 |
| allow | deny | — | 拒绝 |
| allow | confirm | —（不查询） | 人工确认（强制） |
| confirm | — | 无 | 人工确认 |
| confirm | — | 命中 | 放行 |
| confirm | allow | — | 放行（背书） |
| deny | 任意 | 任意 | 拒绝 |

策略源经咨询链逐个同步咨询：具名源（`WithNamedPolicies`）在前、匿名源（`WithPolicies`）在后，各段保持注册次序。处置与咨询顺序无关，但首个 deny / 首个 allow 的来源归属依赖此次序，来源名进入授权终态事件（§7.5）。

### 4.2 规则性质

- **确定性**：处置是（层级、判定集、许可表）的纯函数，与策略源顺序无关。
- **deny 绝对**：层级 `deny` 或任一 `deny` 判定的调用不可经判定、许可或人工决定放行。
- **升级与降级分离**：策略源只能升压（allow→confirm、任意→deny）或背书降级（confirm→allow）；许可只能满足基线确认；人工决定是最终裁决。
- **fail-safe**：策略源咨询出错时，按该源给出 `confirm` 判定处理并记录日志——裁决权威不可达时不静默放行。业务需要 fail-open 时，可在自身实现内吞掉错误并返回 `abstain`。
- **同步关键路径**：策略源咨询与许可查询发生在工具调用线程内，源应低延迟或由业务侧缓存。

### 4.3 许可

- 键：`(UserID, ToolName)`，作用域为会话级（绑定单个会话）或全局级（该用户全部会话）。
- 产生：人工同意且决定显式指定 Remember（会话级/全局级）时写入；拒绝不产生许可。
- 查询：评估步 5 按「会话级 → 全局级」次序，首个命中生效。
- 存储：许可源接口由业务实现（如用户设置系统）；库附带线程安全的内存实现，供测试与示例。
- 生命周期归业务管理（列出/吊销/过期），库仅在步 5 读取、在 Remember 决定时写入。

## 5. 能力行为

### 5.1 人工确认流（直接路径）

评估得出「确认」后，网关完成四步：登记内存等待者 → 持久化待确认指针 → 经原始运行向 SSE 流推送 pending 确认事件（同时落入 track 持久化）→ 阻塞。决定到达后清除指针、推送终态确认事件、唤醒运行：同意则工具在原运行内继续执行，事件仍走原 SSE/follow 流；拒绝则框架向模型返回 denied 结果，工具不执行。

决定携带 Remember 且为同意时，网关同时向许可源写入对应作用域的许可；写入失败不回退本次许可，仅记录日志。未配置许可源时 Remember 被忽略并记录日志。

放行（层级、背书、许可）与拒绝（层级、判定）的调用不产生确认事件：执行经标准 AG-UI 工具事件可见，拒绝经 denied 结果形状可见。

### 5.2 恢复路径（孤儿确认）

决定到达时无存活等待者，网关以存储为准判定：待确认指针存在即为孤儿确认。处理流程：清除指针 → 从会话历史还原原始工具调用（工具名与参数）→ 同意则在服务端代执行工具、拒绝则合成 denied 结果 → 以 RoleTool 消息拉起一次续跑运行，事件落入 track，前端经 `/history` follow 流读取。

历史中找不回原始调用、或工具在裁决时刻不可注册时，降级为拒绝续跑（安全优先，不执行未知调用）。恢复是**基于会话历史的软恢复**（模型重读历史后继续），非执行现场无损恢复。

前端对两条路径无感知差异，统一只调用确认决定接口；实际生效路径由该接口的返回值区分。

### 5.3 状态机与事件

确认状态：`pending → approved | denied`，终态后不再变更。每次状态变更向 AG-UI 流发送同名自定义事件，前端按 `confirmationId` 配对渲染。pending 事件随 track 持久化，历史回放完整重现确认交互过程。

## 6. 仓库布局

```
ack/                      模块根，包 ack：网关构造、选项、对外方法
├── ack.go                Options 与 New、Gateway 生命周期
├── permission.go         层级、判定、许可评估与授权终态观察类型
├── policy.go             框架权限策略适配：评估入口，确认流登记等待者并阻塞，操作性终态结算与观察挂点
├── confirm.go            Decide / Pending 及其请求、结果类型
├── recovery.go           恢复路径：历史还原、代执行、续跑
├── grants.go             许可类型、许可源接口与内存实现
├── payload.go            wire 与存储契约：事件名、状态常量、StateKey、Payload
├── errors.go             哨兵错误
├── doc.go                包文档：能力概述与接入指引
├── ackhttp/              HTTP 绑定：确认路由挂载（仅依赖标准库与 ack）
├── acktest/              测试夹具：脚本化模型（model.Model 实现）、假续跑 runner
├── examples/notes/       端到端示例：笔记工具、层级配置、许可演示、Web 前端（Vue3，产物不入库，占位页 + go:embed）；工具接入双模式——function（进程内）与 mcp（同一批工具经 MCP server 以 streamable HTTP 接入），验证 §4.1 的同管线主张
└── docs/                 事件契约与许可模型文档（前端/业务对接参考）
```

## 7. 对外 API（Go）

### 7.1 构造与选项

```go
func New(svc session.Service, opts ...Option) *Gateway
```

`svc` 为会话存储（必填，后端由业务选择：框架自带 inmemory / postgres 等）。选项：

| 选项 | 默认 | 说明 |
| --- | --- | --- |
| `WithApp(name)` | `"ack"` | 应用名，会话存储键组成部分，须与 AG-UI runner 的 AppName 一致 |
| `WithUser(id)` | `"user"` | 固定用户 ID，会话存储键与许可查询的组成部分 |
| `WithUserFunc(fn)` | 无 | 动态身份：`fn(ctx, threadID) string`，从请求上下文取鉴权结果，供多用户场景 |
| `WithDefaultTier(t)` | `TierAllow` | 未分级工具的层级 |
| `WithTiers(m)` | 无 | 按工具名静态分级（`map[string]Tier`） |
| `WithTierFunc(fn)` | 无 | 动态分级：`fn(ctx, ToolCall) Tier`，可依据入参分类；优先于静态表，静态表优先于默认层级 |
| `WithPolicies(src...)` | 无 | 匿名策略源，每次工具调用逐个同步咨询 |
| `WithNamedPolicies(ps...)` | 无 | 具名策略源，名字进入授权终态事件的 `Source`；与 `WithPolicies` 合成咨询链（具名在前、匿名在后），空名字视同匿名 |
| `WithPermissionObserver(ob)` | nil | 授权终态观察者，订阅全部自动授权终态（人工决定不通知）；调用契约见 §7.5 |
| `WithGrants(gs)` | 无 | 许可源；缺省时评估步 5 视为永不命中，Remember 被忽略 |
| `WithTools(res)` | 无 | 工具解析器：裁决时刻返回 `map[string]tool.CallableTool`，供恢复路径代执行；缺省时同意的孤儿确认降级为"工具不可用"拒绝 |
| `WithStaticTools(tools...)` | 无 | `WithTools` 的静态糖 |
| `WithDenyTexts(t)` | 内置文案 | 各类拒绝文案的分组定制入口（`DenyTexts`）；零值字段保持默认（英文、含"不得重试"指令） |
| `WithLogger(*slog.Logger)` | `slog.Default()` | 结构化日志 |

`Gateway` 并发安全，可被多个 goroutine 同时调用。

### 7.2 类型

```go
// 层级与判定
type Tier string     // TierAllow | TierConfirm | TierDeny
type Verdict string  // VerdictAbstain | VerdictAllow | VerdictConfirm | VerdictDeny

// ToolCall 是评估与分级看到的调用视图
type ToolCall struct {
    SessionID  string
    ToolCallID string
    ToolName   string
    Arguments  json.RawMessage
}

// PolicySource 是外部裁决系统（如风控）的接入点
type PolicySource interface {
    Consult(ctx context.Context, call ToolCall) (Verdict, error)
}

// NamedPolicy 是带名字的策略源：名字进入授权终态事件的 Source
type NamedPolicy struct {
    Name   string
    Source PolicySource
}

// 许可
type Scope string // ScopeSession | ScopeGlobal
type Grant struct {
    UserID    string // 归属用户
    SessionID string // 非空即会话级；空即全局级
    ToolName  string
    CreatedBy string // 决定来源（人工决定的 DecidedBy）
    CreatedAt int64  // 毫秒时间戳
}

// GrantSource 是许可存储的接入点；查询须按会话级先于全局级实现
type GrantSource interface {
    Lookup(ctx context.Context, q GrantQuery) (Grant, bool)
    Save(ctx context.Context, g Grant) error
}
type GrantQuery struct{ UserID, SessionID, ToolName string }

// MemoryGrants 返回线程安全的内存许可源（测试与示例用）
func MemoryGrants() GrantSource
```

### 7.3 方法契约

```go
// PolicyFor 返回单个会话的权限策略：该会话的每次工具调用先经许可评估，
// 评估为确认的调用进入人工确认流程。供直接使用 core runner 的业务挂载。
func (g *Gateway) PolicyFor(sessionID string) tool.PermissionPolicyFunc

// RunOptionResolver 把许可策略适配为 aguirunner.WithRunOptionResolver 的形式，
// 使每次 AG-UI 运行（含恢复路径的续跑）都挂上该策略。
func (g *Gateway) RunOptionResolver() func(ctx context.Context, in *adapter.RunAgentInput) ([]agent.RunOption, error)

// Bind 注入恢复路径续跑使用的 AG-UI runner；传入 nil 返回错误。
// 恢复路径触发时未 Bind 的，Decide 返回 ErrNotBound。
func (g *Gateway) Bind(r ResumeRunner) error

// Decide 结算一次人工决定，返回实际生效的路径（PathLive / PathRecovery）。
// ctx 约束持久化与续跑的启动，不等待续跑完成。
func (g *Gateway) Decide(ctx context.Context, d Decision) (DecideResult, error)

// Pending 列出会话中等待决定的确认项，合并内存等待者与持久化记录；
// Live 标记运行是否存活（false 即孤儿）。
func (g *Gateway) Pending(ctx context.Context, sessionID string) ([]PendingItem, error)
```

`Decision` 与 `DecideResult` 为结构体参数，后续以增字段方式演进：

```go
type Decision struct {
    SessionID      string // 会话 ID
    ConfirmationID string // 确认 ID
    Approved       bool   // true=同意，false=拒绝
    DecidedBy      string // 决定人；留空取网关身份
    Remember       Scope // 空=不生成许可；同意时按作用域写入许可源
}

type DecideResult struct {
    Path string // PathLive = "live"，PathRecovery = "recovery"
}
```

阻塞语义（`PolicyFor` 内部，对业务透明）：权限策略阻塞受运行 ctx 约束，运行取消或超时即按拒绝结算并清理指针。

### 7.4 错误

| 哨兵错误 | 语义 |
| --- | --- |
| `ErrNotFound` | 确认或会话不存在；含已决定（或已取消）后的重复决定与并发决定中的后到者 |
| `ErrNotBound` | 恢复路径需要续跑 runner 但未 `Bind` |
| `ErrRunActive` | 续跑时会话上仍有运行进行（409 场景），对框架运行冲突错误包装，`errors.Is` 可判 |

HTTP 层依据哨兵错误映射状态码，不解析错误字符串。

### 7.5 授权终态观察

```go
// Basis 是授权终态的生效依据词表
type Basis string // tier | verdict | endorsement | grant | duplicate | canceled | persist-failed

type PermissionEvent struct {
    Call    ToolCall // 发生授权的调用视图
    Allowed bool     // 终态是否放行
    Basis   Basis    // 生效依据
    Source  string   // 生效具名策略源名；非策略依据与匿名源为空
}

type PermissionObserver func(ctx context.Context, event PermissionEvent)
```

- 互斥覆盖：每次工具调用恰好一条授权终态——要么一条 `PermissionEvent`，要么一次 Decide 结算（live 唤醒与 recovery 代执行均是），二者互斥且共同覆盖全部调用；人工决定由宿主在 Decide 自身的路由与日志中审计。
- `Source` 口径：`verdict` / `endorsement` 时为首个生效具名策略源的名字，其余依据（tier / grant / duplicate / canceled / persist-failed）与匿名策略源为空；策略源咨询出错 fail-safe 进入确认的调用若以 `canceled` 结算，事件不携带出错源名字（细节在日志）。
- 调用方式：网关在授权流程结束的同步路径上回调，回调应快速返回，重负载外报由宿主异步化；回调 ctx 经 `context.WithoutCancel` 处理、保证未取消（取消/超时终态亦然），回调内可安全执行 IO；回调无返回值，观察不改变授权结论。
- 范围：`PolicyFor` 的防御性放行（无调用标识的请求）不在观察契约内。

## 8. 对外契约

### 8.1 AG-UI 事件契约（前端对接）

确认事件为 AG-UI `CustomEvent`，仅人工确认路径产生；前端按事件名 + `confirmationId` 消费：

- 事件名：`tool_confirmation`
- value 即 `Payload`：

```json
{
  "confirmationId": "call_1",
  "toolCallId": "call_1",
  "toolName": "delete_note",
  "arguments": {"note_id": "n1"},
  "sessionId": "t1",
  "status": "pending",
  "decidedBy": "",
  "createdAt": 1760000000000
}
```

- 配对规则：同一 `confirmationId` 收到多条同名事件，`status` 单调 `pending → approved|denied`，终态事件至多一条。
- 实时性：pending 与终态事件经 SSE 流实时下发；重连后经 `/history` 回放补齐。
- 孤儿补偿：服务重启后终态不再有实时流，前端以待确认查询的结果（`live=false`）渲染等待中的孤儿确认，决定后经 `/history` follow 流读取续跑结果。

### 8.2 HTTP 绑定（ackhttp）

```go
// Mount 把确认路由挂载到 mux；前缀默认 "/ack"。
func Mount(mux *http.ServeMux, g *ack.Gateway, opts ...Option) // WithPrefix(p string)
```

| 路由 | 请求 | 成功响应 | 错误 |
| --- | --- | --- | --- |
| `POST {p}/confirmation` | `{"threadId","confirmationId","approved","remember?"}` | `200 {"path":"live"\|"recovery"}` | 400 形状缺失或 `remember` 非法；404 `ErrNotFound`；409 `ErrRunActive`；500 |
| `GET {p}/confirmations?threadId=` | — | `200 [PendingItem]` | 400 缺 threadId；500 |

`remember` 可选，取 `"session"` / `"global"`，语义同 `Decision.Remember`：同意时生成对应作用域许可，拒绝时忽略。身份从请求上下文解析：鉴权中间件将用户写入 ctx，`WithUserFunc` 从 ctx 取值；未配置动态身份时使用静态 `WithUser` 值。

### 8.3 待确认指针存储契约

- 键：`Session.State["__pending_tool_confirmation__"]`（`StateKey`）。
- 值：JSON 对象 `{"<confirmationID>": Payload}`。
- 写入时机：进入等待前（先持久化后阻塞）；清除时机：决定结算、运行终止清理、会话删除。清除失败时残留孤儿指针（记 Error 日志），其后的人工决定按恢复路径既定语义结算（I13）。
- 读写模式：网关锁内 read-modify-write，单进程下串行一致。

`StateKey` 与事件名是存储/wire 兼容键：升级只增不改，否则遗留待确认指针失联、前端配对失效。

### 8.4 拒绝与失败结果形状

拒绝（层级 `deny`、策略 `deny` 判定、人工拒绝、运行终止）与失败（工具不可用、执行失败）统一以对齐框架 `tool.PermissionResult` 的 JSON 形状返回给模型：

```json
{"status": "denied", "tool": "delete_note", "reason": "User denied the execution of tool delete_note. Do not call this tool again."}
```

同意的孤儿确认代执行后，工具结果 JSON 序列化为 RoleTool 消息内容。直接路径拒绝（权限策略拒绝）与恢复路径拒绝（续跑运行中的拒绝结果）形状一致，模型理解无差异。

## 9. 不变量

| # | 不变量 |
| --- | --- |
| I1 | 未获放行依据（层级 allow、背书、许可、人工同意）的工具调用不执行；一切拒绝型结果对模型包含"不得重试"指令。 |
| I2 | 许可评估是纯函数：给定相同层级、判定集与许可表，处置唯一，且与策略源顺序无关。 |
| I3 | deny 不可降级：层级 `deny` 或任一 `deny` 判定的调用，任何判定、许可与人工决定都不能使其执行。 |
| I4 | 强制确认不可被许可满足，必须由人工决定。 |
| I5 | 许可只能产生于人工同意且 Remember 显式指定；拒绝不产生许可。 |
| I6 | 许可查询次序固定：会话级先于全局级。 |
| I7 | 确认 ID 即工具调用 ID，会话内唯一；等待者以（会话 ID, 确认 ID）隔离命名空间。 |
| I8 | 进入等待前必须成功持久化待确认指针；持久化失败按拒绝结算，工具不执行。 |
| I9 | 运行取消或超时，其全部未决确认清理指针并按拒绝结算。 |
| I10 | 确认状态机 `pending → approved\|denied` 终态唯一；终态事件按 `confirmationId` 与 pending 事件配对。 |
| I11 | 同一确认的并发决定，先到者生效，后到者返回 `ErrNotFound`。 |
| I12 | 同会话出现重复确认 ID 时，后到请求直接放行，不进入确认流程。 |
| I13 | 孤儿判定以存储为准：无存活等待者时查待确认指针，存在走恢复路径，不存在返回 `ErrNotFound`。 |
| I14 | 网关不拥有会话存储、许可源与 runner 的生命周期，三者由业务构造与关闭。 |
| I15 | 网关的 AppName/User 须与 AG-UI runner 同名配置一致，否则会话定位失败。 |
| I16 | 每次工具调用恰好一条授权终态：自动终态产生一条 `PermissionEvent`，人工决定经 Decide 结算，二者互斥。取消与决定并发时先认领结算权者生效：决定生效则零事件、取消生效则决定返回 `ErrNotFound` 且恰一条 canceled 事件。 |

## 10. 依赖与部署约束

| # | 约束 |
| --- | --- |
| D1 | 单进程部署：确认决定与运行须同实例；多实例需外围会话粘性（范围外）。 |
| D2 | 优雅停机：进程强杀可能丢失 flush 周期内的最后一条事件。 |
| D3 | AG-UI runner 必须配置运行超时：未决确认最终由超时结算为拒绝。 |
| D4 | 上游 trpc-agent-go（截至 v1.11.2）的 TrackEvent 未保证时间戳严格单调：AG-UI history follow 以"严格大于游标"过滤事件，时间戳并列会使同簇后续事件（含运行结束信号）被永久跳过。本库直依赖官方发布、不做 replace：该缺陷只影响前端 follow 消费（库自身不消费 follow 流），缓解下沉到示例层——前端在 recovery 决定后延时再开 history follow，Go 集成测试以快照轮询断言、不依赖 follow 自然终止（对 D4 免疫）；Windows 粗时钟下快照切簇竞态偶发，验收以 Linux 为准。彻底消除需 fork + replace（逃生舱）；上游修复发布后移除示例缓解并回归本条。 |
| D5 | 无 CGO 依赖；会话后端由业务自选（框架 inmemory / postgres 等）。 |
| D6 | 策略源咨询与许可查询位于工具调用的同步关键路径：源应低延迟或由业务侧缓存；源错误按 fail-safe 升级为确认处理。 |
| D7 | 上游 trpc-mcp-go（截至 v0.0.19）streamable 客户端存在会话状态并发访问缺陷：GET SSE goroutine 建连期间读 sessionID、收流期间写 lastEventID，与 POST 响应处理中的写入无同步（streamable_client.go），`go test -race ./...` 多包并行负载下偶发数据竞争。本库以 `replace trpc.group/trpc-go/trpc-mcp-go => github.com/JNinng/trpc-mcp-go v0.0.19-3` 接入 fork 修复（`sessionStateMutex` 统一守护 sessionID / isStateless / lastEventID），示例恢复默认 GET SSE，此前的 `WithClientGetSSEEnabled(false)` 缓解已移除；上游合入修复后改回官方依赖并回归本条。 |

## 11. 可观测性

- 结构化日志（`slog`），事件域覆盖：许可评估结果（含命中步骤与依据）、策略源咨询错误（含来源名）、许可写入失败、确认登记、持久化失败、决定结算、恢复路径降级、续跑启动失败。
- 授权终态经 `WithPermissionObserver` 外报宿主（同步回调，契约见 §7.5），人工决定归宿主在 Decide 路由中审计。
- 确认全生命周期经 AG-UI track 持久化，历史回放即审计轨迹；放行与拒绝路径经标准工具事件与 denied 结果形状可见。
- 库内不引入 metrics 依赖；业务可按需采集 `Decide` 返回的 Path 做路径分布统计。

## 12. 测试要求

**许可模型（单元测试）**

- 分级解析优先级：TierFunc > 静态表 > 默认层级。
- 评估矩阵全分支：§4.1 表逐行覆盖（含 deny 绝对、背书降级、强制确认）。
- 许可查询次序：会话级先于全局级；无许可源时步 5 永不命中。
- Remember 写通：同意 + 作用域生成对应许可；拒绝不生成；写入失败不影响本次许可。
- 策略源错误 fail-safe：按 `confirm` 判定处理并记日志。

**确认流（acktest 夹具，无真实 LLM）**

- 直接路径：同意唤醒并执行、拒绝不执行、运行取消清理、跨会话相同确认 ID 隔离。
- 恢复路径：孤儿同意代执行并续跑、孤儿拒绝合成结果、历史缺失降级拒绝、未 Bind 报 `ErrNotBound`。
- 并发：同一确认并发决定竞争（I11）。
- 错误：哨兵错误的 `errors.Is` 判定。

**授权终态观察（单元测试，经 `PolicyFor` 直接驱动）**

- 观察契约全场景（§7.5）：分级放行/拦截、判定拒绝、背书、许可命中、评估层确认不通知、Decide 结算不通知、重复 ID、持久化失败、取消结算。
- 来源归属：多源时事件 `Source` 为首个生效具名源、匿名源与空名具名源为空、咨询链具名在前匿名在后。
- 互斥不变量（I16）：取消与决定并发时决定优先，恰好一条终态；观察者缺省 nil 零行为；回调 ctx 保证未取消。

**HTTP 集成测试（ackhttp）**

- 决定/查询路由的请求校验（含 `remember` 字段）、状态码、响应形状；409 包装正确。

**示例工程验收（examples）**

- 允许层级直接执行、确认层级出卡片、remember 后同会话免确认、实时确认卡片、历史回放、页面刷新恢复、服务重启恢复端到端通过（重启恢复以 Linux 为准，Windows 偶发竞态见 D4）；deny 层级不可降级与策略源三形态（强制确认、背书、deny 判定）有演示场景。
- 工具接入双模式（function / MCP）在核心场景（allow 直行、确认出卡、deny 拒绝、remember 免确认、重启恢复代执行）下行为一致，有 MCP 模式对比测试。

## 13. 版本与兼容性

- 语义化版本；v1 起 Go API 与 wire 契约进入稳定期。
- wire 契约（事件名、Payload 字段、StateKey、HTTP 路由形状）只增不改：新增字段必须可被旧消费方忽略，字段语义不得变更。
- Go API 以结构体参数（`Decision`、`ToolCall`、`Grant` 等）承载演进空间，增字段不破坏调用方。
- 依赖的 trpc-agent-go 版本升级须回归 D4 约束与全部测试要求。

## 14. 最简接入示例

```go
svc := inmemory.NewSessionService() // 或 postgres 等框架后端

ag := llmagent.New("biz",
    llmagent.WithModel(openai.New(cfg.Model, openai.WithAPIKey(cfg.Key))),
    llmagent.WithTools(readNote, deleteNote),
    llmagent.WithMaxToolIterations(8), // 拒绝后的重试兜底
)

gw := ack.New(svc,
    ack.WithApp("biz"),                // 须与 runner 的 AppName 一致
    ack.WithUser("u1"),
    ack.WithTiers(map[string]ack.Tier{ // 未分级工具默认 allow
        "delete_note": ack.TierConfirm,
    }),
    ack.WithPolicies(riskControl),     // 策略源：可 deny / 强制 confirm / 背书
    ack.WithGrants(ack.MemoryGrants()),// 业务可用自有存储实现
    ack.WithStaticTools(readNote, deleteNote), // 恢复路径代执行用
)

r := aguirunner.New(runner.NewRunner("biz", ag, runner.WithSessionService(svc)),
    aguirunner.WithAppName("biz"),
    aguirunner.WithSessionService(svc),
    aguirunner.WithRunOptionResolver(gw.RunOptionResolver()),
    aguirunner.WithMessagesSnapshotFollowEnabled(true),
    aguirunner.WithTimeout(10*time.Minute), // D3
)
_ = gw.Bind(r)

srv, _ := agui.New(/* core runner 与 runnerFactory 返回 r */)

mux := http.NewServeMux()
ackhttp.Mount(mux, gw) // POST /ack/confirmation、GET /ack/confirmations
mux.Handle("/agui/", http.StripPrefix("/agui", srv.Handler()))
```

前端约定：消费 `tool_confirmation` 事件渲染确认卡片，决定时调用 `POST /ack/confirmation`（可携带 `remember`），重连时以待确认查询补偿孤儿状态，续跑结果经 `/agui/history` follow 流读取。
