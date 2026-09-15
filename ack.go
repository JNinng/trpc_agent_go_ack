package ack

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"

	"trpc.group/trpc-go/trpc-agent-go/server/agui/adapter"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// ToolResolver 在裁决时刻返回"工具名 → 可调用工具"注册表，供恢复路径
// 代执行使用。做成函数形式使 MCP 等动态工具集可以在决定时刻实时解析。
type ToolResolver func(ctx context.Context) map[string]tool.CallableTool

// ResumeRunner 与 AG-UI runner 的运行接口一致，恢复路径用它拉起续跑
// 运行。aguirunner.New 的返回值直接满足该接口。
type ResumeRunner interface {
	Run(ctx context.Context, runAgentInput *adapter.RunAgentInput) (<-chan aguievents.Event, error)
}

// eventEmitter 抽象 AG-UI 运行的事件推送能力
// （运行存活时为 *aguirunner.Run，否则为 nil）。
type eventEmitter interface {
	Emit(ctx context.Context, event aguievents.Event) error
}

// waiter 是一次阻塞中的确认：评估侧阻塞等待 decided，
// Decide 侧写入决定后唤醒。
type waiter struct {
	payload Payload
	run     eventEmitter // 运行存活时非 nil，用于向原始流推送确认事件
	decided chan bool    // 缓冲为 1 的决定通道：true=同意，false=拒绝
}

// Gateway 是库的核心对象：评估工具调用处置（放行/确认/拒绝），并承载
// 人工确认的完整生命周期——登记等待者、持久化待确认指针、结算决定、
// 恢复孤儿确认。Gateway 并发安全，可被多个 goroutine 同时调用。
//
// 网关不拥有会话存储、许可源与续跑 runner 的生命周期，三者由业务
// 构造与关闭。
type Gateway struct {
	opts     *options
	sessions session.Service

	mu       sync.Mutex
	waiters  map[string]*waiter  // 存活等待者表；键为 waiterKey(sessionID, confirmationID)
	settling map[string]struct{} // 结算中的确认；并发决定先到者生效
	resume   ResumeRunner        // 恢复路径续跑使用的 AG-UI runner
}

// options 汇集 Gateway 的全部配置项。
type options struct {
	app         string
	user        string
	userFunc    func(ctx context.Context, threadID string) string
	defaultTier Tier
	tiers       map[string]Tier
	tierFunc    func(ctx context.Context, call ToolCall) Tier
	policies    []PolicySource
	grants      GrantSource
	tools       ToolResolver
	denyText    func(call ToolCall) string
	logger      *slog.Logger
}

// Option 配置 Gateway。
type Option func(*options)

// WithApp 设置应用名（默认 "ack"）。它是会话存储键的组成部分，
// 必须与 AG-UI runner 的 AppName 一致，否则会话定位失败。
func WithApp(name string) Option {
	return func(o *options) { o.app = name }
}

// WithUser 设置固定用户 ID（默认 "user"），是会话存储键与许可查询的
// 组成部分。多用户场景改用 WithUserFunc。
func WithUser(id string) Option {
	return func(o *options) { o.user = id }
}

// WithUserFunc 设置动态身份：从请求上下文取鉴权结果
// （鉴权中间件将用户写入 ctx）。优先于 WithUser。
func WithUserFunc(fn func(ctx context.Context, threadID string) string) Option {
	return func(o *options) { o.userFunc = fn }
}

// WithDefaultTier 设置未分级工具的层级（默认 TierAllow）。
func WithDefaultTier(t Tier) Option {
	return func(o *options) { o.defaultTier = t }
}

// WithTiers 按工具名静态分级。
func WithTiers(m map[string]Tier) Option {
	return func(o *options) { o.tiers = m }
}

// WithTierFunc 设置动态分级，可依据入参分类；优先于静态表，
// 静态表优先于默认层级。
func WithTierFunc(fn func(ctx context.Context, call ToolCall) Tier) Option {
	return func(o *options) { o.tierFunc = fn }
}

// WithPolicies 设置策略源链，每次工具调用逐个同步咨询。
func WithPolicies(src ...PolicySource) Option {
	return func(o *options) { o.policies = src }
}

// WithGrants 设置许可源；缺省时许可评估视为永不命中，Remember 被忽略。
func WithGrants(gs GrantSource) Option {
	return func(o *options) { o.grants = gs }
}

// WithTools 设置工具解析器：恢复路径代执行时调用，返回"工具名 →
// 可调用工具"。缺省时同意的孤儿确认降级为"工具不可用"拒绝。
func WithTools(res ToolResolver) Option {
	return func(o *options) { o.tools = res }
}

// WithStaticTools 是 WithTools 的静态糖：直接给出工具集合，
// 裁决时刻按声明名解析。
func WithStaticTools(tools ...tool.Tool) Option {
	return func(o *options) { o.tools = staticToolResolver(tools) }
}

// WithDenyText 自定义人工拒绝时返回给模型的提示文案；默认文案含
// "不得重试"指令，自定义文案须自行保留该约束。
func WithDenyText(fn func(call ToolCall) string) Option {
	return func(o *options) { o.denyText = fn }
}

// WithLogger 设置结构化日志器（默认 slog.Default()）。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// New 创建许可网关。svc 为会话存储，由业务选择后端（框架自带
// inmemory / postgres 等）并负责其生命周期。
func New(svc session.Service, opts ...Option) *Gateway {
	if svc == nil {
		panic("ack: session.Service is required")
	}
	o := &options{
		app:         "ack",
		user:        "user",
		defaultTier: TierAllow,
		denyText:    defaultDenyText,
		logger:      slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	return &Gateway{
		opts:     o,
		sessions: svc,
		waiters:  map[string]*waiter{},
		settling: map[string]struct{}{},
	}
}

// Bind 注入恢复路径续跑使用的 AG-UI runner；传入 nil 返回错误。
// 恢复路径触发时未 Bind 的，Decide 返回 ErrNotBound。
func (g *Gateway) Bind(r ResumeRunner) error {
	if r == nil {
		return errors.New("ack: nil resume runner")
	}
	g.mu.Lock()
	g.resume = r
	g.mu.Unlock()
	return nil
}

// resolveUser 解析会话归属用户：优先动态身份（WithUserFunc），否则取
// 静态 WithUser 值。
func (g *Gateway) resolveUser(ctx context.Context, threadID string) string {
	if g.opts.userFunc != nil {
		return g.opts.userFunc(ctx, threadID)
	}
	return g.opts.user
}

// waiterKey 把确认 ID 按会话隔离命名空间：不同会话可能复用同一个
// 工具调用 ID。
func waiterKey(sessionID, confirmationID string) string {
	return sessionID + "/" + confirmationID
}

// staticToolResolver 把静态工具集合适配为 ToolResolver。
func staticToolResolver(tools []tool.Tool) ToolResolver {
	return func(context.Context) map[string]tool.CallableTool {
		reg := map[string]tool.CallableTool{}
		for _, t := range tools {
			ct, ok := t.(tool.CallableTool)
			if !ok {
				continue
			}
			if d := ct.Declaration(); d != nil && d.Name != "" {
				reg[d.Name] = ct
			}
		}
		return reg
	}
}
