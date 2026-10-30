# ack 原理机制

本文从实现视角说明 ack 库各组件的机制与原理；接入步骤与配置项见 [usage.md](./usage.md)，完整设计契约（事件、存储、不变量）见 [module_design.md](./module_design.md)。

## 一、总体架构

`ack` 构建在 trpc-agent-go 的 AG-UI 协议栈之上，核心是一个 `Gateway`（[ack.go](../ack.go)），它拦截每次工具调用，做出三类处置：

```mermaid
flowchart LR
    A["工具调用"] --> E["许可评估<br/>permission.go"]
    E -->|allow| OK["直接放行执行"]
    E -->|deny| NO["合成拒绝结果<br/>返回模型"]
    E -->|confirm| HUMAN["人工确认流程<br/>confirm.go / recovery.go"]
    HUMAN -->|"live 路径（运行仍存活）"| LIVE["阻塞原运行<br/>等待决定唤醒"]
    HUMAN -->|"recovery 路径（运行已不存在，<br/>如服务重启后的孤儿确认）"| REC["代执行/合成结果并续跑<br/>结果走 /history follow 流"]
    LIVE -.- N1["含义：决定直接唤醒阻塞中的运行，<br/>事件仍走原 SSE 流，工具在原运行内执行"]
    REC -.- N2["含义：网关依据持久化指针软恢复，<br/>模型重读历史后继续，非现场无损恢复"]
```

- **live 路径**：发出确认的运行仍存活。决定经 `decided` 通道直接唤醒阻塞中的运行，工具在原运行内继续执行，事件仍走原始 SSE/follow 流（§三.1、§三.2 settleLive）。
- **recovery 路径**：运行已不存在（如服务重启后决定才到达的孤儿确认）。网关以待确认持久化指针为准，代执行工具或合成拒绝结果，拉起续跑运行，前端经 `/history` follow 流读取结果（§三.3）。

组件关系：

```mermaid
flowchart TB
    subgraph 宿主应用
        BIZ["业务代码"]
        MUX["http.ServeMux"]
        FE["前端<br/>AG-UI SSE + 确认卡片"]
    end
    subgraph ack 库
        GW["Gateway<br/>ack.go"]
        EV["评估管线<br/>permission.go / policy.go"]
        GR["GrantSource<br/>grants.go"]
        HTTP["ackhttp<br/>路由挂载"]
    end
    subgraph trpc-agent-go
        RUN["aguirunner<br/>AG-UI 运行"]
        SESS["session.Service<br/>会话存储"]
    end
    subgraph 外部
        PS["PolicySource<br/>风控等裁决系统"]
    end

    %% 业务代码的构造与装配
    BIZ -->|"① New(svc, …) 构造网关"| GW
    BIZ -->|"② 构造并持有会话存储<br/>（决定生命周期归属）"| SESS
    BIZ -->|"③ aguirunner.New(coreRunner) 构造续跑 runner"| RUN
    BIZ -->|"④ Bind(r) 注入恢复路径续跑能力"| GW
    BIZ -->|"⑤ RunOptionResolver 交给 runner：<br/>每次运行（含续跑）自动挂许可策略"| RUN
    BIZ -->|"⑥ Mount(mux, gw) 挂载决定/查询路由"| MUX

    %% 请求与数据流
    FE -->|"运行请求 /agui（SSE）"| MUX
    MUX --> RUN
    RUN -.->|"tool_confirmation / 运行事件<br/>下发渲染确认卡片"| FE
    FE -->|"POST /ack/confirmation 提交决定"| MUX
    MUX -->|"路由到 handlers"| HTTP
    HTTP -->|"Decide 结算 / Pending 查询"| GW

    %% 网关的依赖调用
    GW -->|"每次调用逐个同步咨询 Verdict"| EV
    EV -->|"Consult（fail-safe：出错按 confirm）"| PS
    GW -->|"Lookup 评估命中 / Save Remember 许可"| GR
    GW -->|"待确认指针读写 savePending"| SESS
    GW -->|"settleRecovered 拉起续跑运行"| RUN
    RUN -->|"事件持久化到会话 track"| SESS
```

连线语义速查（编号对应图中①~⑥）：

| 线 | 语义 | 代码位置 |
| --- | --- | --- |
| ① | 业务构造网关，传入会话存储与全部配置 | [ack.go](../ack.go#L164) `New` |
| ② | 会话存储由业务选择后端并持有生命周期，网关只使用 | [ack.go](../ack.go#L42-L45) |
| ③ | 业务构造 AG-UI runner（包装 core runner），其返回值满足 `ResumeRunner` | [ack.go](../ack.go#L22-L24) |
| ④ | 网关注入续跑 runner，恢复路径触发时使用，未 Bind 返回 `ErrNotBound` | [ack.go](../ack.go#L208) `Bind` |
| ⑤ | 网关的 `RunOptionResolver` 使每次运行挂上 `PolicyFor` 许可策略 | [policy.go](../policy.go#L48-L55) |
| ⑥ | ackhttp 把决定/查询路由挂到宿主 mux | [ackhttp/http.go](../ackhttp/http.go#L22) `Mount` |

组件分工：

| 组件 | 文件 | 职责 |
| --- | --- | --- |
| Gateway | [ack.go](../ack.go) | 核心对象：登记等待者、结算决定、恢复孤儿确认；并发安全 |
| 评估管线 | [permission.go](../permission.go) / [policy.go](../policy.go) | 层级解析、策略咨询、处置决策、观察者回调 |
| 许可（Grant） | [grants.go](../grants.go) | 用户预授权存储抽象，满足确认要求 |
| 确认结算 | [confirm.go](../confirm.go) | Decide 结算（live/recovery 分流）、Pending 查询、Remember 写许可 |
| 恢复路径 | [recovery.go](../recovery.go) | 孤儿确认：从会话历史还原调用、代执行工具、拉起续跑 |
| 载荷契约 | [payload.go](../payload.go) | `tool_confirmation` 自定义事件、Session.State 待确认指针 |
| HTTP 绑定 | [ackhttp/http.go](../ackhttp/http.go) | `POST /ack/confirmation`、`GET /ack/confirmations` |
| 测试夹具 | [acktest/](../acktest/) | 脚本化模型、假续跑 runner |

## 二、许可评估管线

评估入口是 `Gateway.evaluate`（[permission.go](../permission.go#L184-L218)），它是（层级、判定集、许可表）的**纯函数**，按固定次序短路：

```mermaid
flowchart TD
    S["工具调用"] --> T{"层级 deny？"}
    T -->|是| D1["拒绝 · tier"]
    T -->|否| C1{"策略源 deny 判定？"}
    C1 -->|是| D2["拒绝 · verdict<br/>（deny 绝对，不可降级）"]
    C1 -->|否| C2{"策略源 confirm 判定？"}
    C2 -->|是| F["强制确认 · forced<br/>（Grant 不可满足）"]
    C2 -->|否| T2{"层级 allow？"}
    T2 -->|是| A1["放行 · tier"]
    T2 -->|否| C3{"策略源 allow 判定？（背书）"}
    C3 -->|是| A2["放行 · endorsement<br/>confirm 降级为放行"]
    C3 -->|否| G{"Grant 命中？<br/>会话级先于全局级"}
    G -->|是| A3["放行 · grant"]
    G -->|否| BASE["基线确认 · baseline<br/>进入人工确认流程"]
```

判定次序逐条说明：

1. **层级 deny** → 拒绝（依据 `tier`）
2. **任一策略源 deny 判定** → 拒绝（依据 `verdict`，来源为首个 deny 的具名源）——deny 绝对，不可降级
3. **任一策略源 confirm 判定** → 强制确认（内部依据 `forced`），许可查询不会发生——强制确认不可被 Grant 满足
4. **层级 allow** → 放行（依据 `tier`）
5. **任一策略源 allow 判定**（背书）→ confirm 层级降级为放行（依据 `endorsement`）
6. **Grant 命中**（会话级先于全局级）→ 放行（依据 `grant`）
7. 兜底 → **基线确认**（内部依据 `baseline`），进入人工确认流程

要点：

- **策略咨询链**（[permission.go](../permission.go#L161-L176)）：具名源在前、匿名源在后，逐个同步咨询。位于工具调用的同步关键路径。咨询出错按 confirm 判定处理（**fail-safe**：裁决权威不可达时不静默放行）。
- **层级解析优先级**：`TierFunc`（动态，可按入参分类）> `WithTiers` 静态表 > 默认层级。
- **授权终态观察**（`PermissionObserver`）：全部自动放行/拒绝终态（tier / verdict / endorsement / grant / duplicate / canceled / persist-failed）在结算同步路径回调宿主审计；**人工决定不发事件**——每次工具调用恰好一条授权终态，要么观察事件、要么 Decide 结算，互斥且共同覆盖。回调 ctx 经 `context.WithoutCancel` 保证未取消，可安全执行 IO。
- **拒绝文案**均含"不得重试"指令，防止模型反复重试被拒工具。

## 三、人工确认生命周期

### 1. 登记与阻塞（live 路径）

```mermaid
sequenceDiagram
    participant M as 模型（原运行内）
    participant P as 许可策略<br/>PolicyFor
    participant GW as Gateway
    participant ST as Session.State<br/>（待确认指针）
    participant FE as 前端

    M->>P: 工具调用（evaluate 判为 confirm）
    P->>GW: awaitConfirmation
    GW->>GW: 登记 waiter（重复 ID 防御性放行）
    GW->>ST: 先持久化待确认指针
    Note over GW: 持久化失败 → 直接拒绝结算（persist-failed）
    GW->>FE: Emit tool_confirmation（pending）
    GW->>GW: 阻塞等待 decided / ctx 结束
    alt 人工决定（Decide · settleLive）
        GW->>ST: 清除指针
        GW->>GW: Remember 同意 → 写 Grant
        GW->>FE: Emit tool_confirmation（approved/denied）
        GW->>P: decided 通道唤醒
        P->>M: 同意 → 继续执行工具 / 拒绝 → denied 结果
    else 取消 / 超时
        GW->>ST: 清理指针
        P->>M: 拒绝结算（canceled）
    end
```

`awaitConfirmation`（[policy.go](../policy.go#L59-L117)）：

1. 构造 `Payload` 与带缓冲 `waiter`（`decided chan bool`）；从 ctx 提取存活运行（`eventEmitter`）。
2. 以 `waiterKey(sessionID, confirmationID)` 登记；**同会话重复确认 ID 防御性放行**（依据 `duplicate`）。
3. **先持久化后阻塞**：待确认指针写入 `Session.State`（键 `__pending_tool_confirmation__`）成功后才等待；持久化失败直接按拒绝结算（依据 `persist-failed`）——保证重启后孤儿状态可恢复。
4. 向前端推送 `tool_confirmation` 自定义事件（实时 SSE + track 持久化）。
5. 阻塞等待：`decided` 通道唤醒（人工决定）或运行 ctx 结束（取消/超时 → 清理指针，按拒绝结算，依据 `canceled`）。

### 2. Decide 结算

```mermaid
flowchart TD
    DEC["Decide（人工决定到达）"] --> BS{"beginSettle<br/>原子认领结算权"}
    BS -->|被占| NF["ErrNotFound（404）<br/>后到者不生效"]
    BS -->|成功| Q{"存在存活 waiter？"}
    Q -->|"是 → live 路径<br/>（决定直接唤醒原运行）"| LIVE["settleLive<br/>清除指针 → Remember → 终态事件 →<br/>decided 通道唤醒原运行"]
    Q -->|"否（孤儿）→ recovery 路径<br/>（运行已不存在，软恢复续跑）"| REC["settleRecovered<br/>见恢复路径"]
    LIVE -.- N1["含义：工具在原运行内继续执行，<br/>事件仍走原 SSE/follow 流"]
    REC -.- N2["含义：代执行/合成结果并拉起续跑，<br/>前端经 /history follow 流读取"]
```

`Decide`（[confirm.go](../confirm.go#L37-L54)）先原子**认领结算权**（`beginSettle`）：`settling` 表防并发双重结算，后到者返回 `ErrNotFound`（HTTP 404）。按是否存在存活等待者分流：

- **settleLive**（[confirm.go](../confirm.go#L59-L85)）：清除持久化指针 → 写 Remember 许可 → 推送终态确认事件 → 向 `decided` 写入决定唤醒阻塞的运行。同意则工具在原运行内继续执行，事件仍走原 SSE/follow 流；拒绝则框架向模型返回 denied 结果。
- **settleRecovered**：见下节。

### 3. 恢复路径（recovery）

决定到达时运行已不存在（如服务重启），以待确认**持久化指针**为准判定孤儿（[recovery.go](../recovery.go#L26-L98)）：

```mermaid
sequenceDiagram
    participant FE as 前端
    participant GW as Gateway<br/>settleRecovered
    participant ST as Session.State
    participant HIS as 会话事件历史
    participant TL as ToolResolver<br/>+ 工具
    participant RR as ResumeRunner<br/>续跑运行
    participant TR as 会话 track

    FE->>GW: Decide（孤儿确认）
    GW->>ST: 清除待确认指针
    GW->>HIS: findToolCall 还原工具名与参数
    alt 还原失败 / 工具不可注册
        Note over GW: 降级为拒绝（安全优先，不执行未知调用）
        GW->>GW: 合成 denied 结果
    else 同意
        GW->>TL: 代执行工具（裁决时刻实时解析）
        GW->>GW: 工具结果作为 RoleTool 消息内容
    else 拒绝
        GW->>GW: 合成 denied 结果
    end
    GW->>RR: Run（RoleTool 消息拉起续跑）
    RR->>TR: 事件持久化到 track
    FE->>TR: /history follow 流读取续跑结果
```

要点：

1. 清除指针；从**会话事件历史**还原原始工具调用的工具名与参数（`findToolCall`）。
2. **同意**：通过 `ToolResolver` 在服务端**代执行工具**（MCP 等动态工具集在裁决时刻实时解析）；**拒绝**：合成 denied 结果。历史找不回调用或工具不可注册时降级为拒绝续跑（**安全优先**，不执行未知调用）。
3. 以 `RoleTool` 消息拉起一次续跑运行（`ResumeRunner.Run`）；事件由 AG-UI runner 持久化到 track，前端经 `/history` follow 流读取。会话上仍有运行时返回 `ErrRunActive`（HTTP 409）。

这是基于会话历史的**软恢复**（模型重读历史后继续），非执行现场无损恢复。

### 4. Remember 生成许可

人工同意且显式携带 `Remember`（`session`/`global`）时，`rememberGrant` 向 `GrantSource` 写入对应作用域的许可（[confirm.go](../confirm.go#L131-L156)）。写入失败不回退本次放行，仅记日志；未配置许可源时忽略。

```mermaid
flowchart LR
    A["人工同意<br/>remember = session / global"] --> B["rememberGrant<br/>写入 GrantSource"]
    B --> C["下次同类调用<br/>评估管线第 6 步命中"]
    C --> D["放行 · grant<br/>免确认直行"]
```

下次同类调用在评估管线第 6 步命中，免确认。

## 四、并发不变量

- **结算权互斥**：`beginSettle`/`endSettle`（[confirm.go](../confirm.go#L164-L184)）保证同一确认不会被并发双重结算；并发决定先到者生效。
- **身份校验摘除**：操作性结算（取消/超时/持久化失败）仅当登记者正是本调用的等待者才摘除，防止误摘同 ID 后到的新等待者；被人工决定抢先结算时按该决定返回。
- **单写者持久化**：`savePending` 在网关锁内做"读取→修改→写回"，单进程串行一致。
- **契约只增不改**：事件名 `tool_confirmation`、State 键、Payload 字段是 wire 与存储的兼容性契约，升级只增不改，否则遗留指针失联。

## 五、接入方式

```go
gw := ack.New(svc, ...)                    // 会话存储由业务持有
r := aguirunner.New(coreRunner,
    aguirunner.WithRunOptionResolver(gw.RunOptionResolver()), // 每次运行（含续跑）挂许可策略
)
_ = gw.Bind(r)                             // 恢复路径续跑 runner
ackhttp.Mount(mux, gw)                     // HTTP 决定/查询路由
```

- **直接用 core runner 的业务**：用 `gw.PolicyFor(sessionID)` 取得 `tool.PermissionPolicyFunc` 挂载。
- **前端约定**：消费 `tool_confirmation` 事件渲染确认卡片 → 调 `POST /ack/confirmation`（可带 `remember`）→ 重连时用 `GET /ack/confirmations` 补偿孤儿状态（`live=false`）→ recovery 结果经 `/agui/history` follow 流读取。
- **身份**：`WithUserFunc` 从请求 ctx 取鉴权结果（优先），否则用 `WithUser` 静态值；用户 ID 参与会话存储键与许可查询。
