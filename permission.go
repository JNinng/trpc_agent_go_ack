package ack

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tier 是工具的基线许可层级。
type Tier string

const (
	// TierAllow 默认放行。
	TierAllow Tier = "allow"
	// TierConfirm 需人工确认；可被背书或已授予的许可满足。
	TierConfirm Tier = "confirm"
	// TierDeny 禁用；不可经判定、许可或人工决定放行。
	TierDeny Tier = "deny"
)

// Verdict 是策略源对一次工具调用返回的意见。
type Verdict string

const (
	// VerdictAbstain 无立场，不参与裁决。
	VerdictAbstain Verdict = "abstain"
	// VerdictAllow 背书：可将 confirm 层级降级为放行。
	VerdictAllow Verdict = "allow"
	// VerdictConfirm 强制确认：必须由人工决定，不可被许可满足。
	VerdictConfirm Verdict = "confirm"
	// VerdictDeny 拒绝：绝对，不可降级。
	VerdictDeny Verdict = "deny"
)

// Basis 是授权终态的生效依据词表：一次工具调用的许可流程结束时，
// 标明放行或拒绝由哪条依据产生。该类型同时用作评估管线的内部依据
// （evaluate 的返回值，供日志输出），内部值 basisForced / basisBaseline
// 保持未导出、不进入终态事件。
type Basis string

const (
	// BasisTier 层级依据：工具的许可层级直接放行或禁用。
	BasisTier Basis = "tier"
	// BasisVerdict 判定依据：首个 deny 判定的策略源拒绝。
	BasisVerdict Basis = "verdict"
	// BasisEndorsement 背书依据：首个 allow 判定的策略源将 confirm
	// 层级降级为放行。
	BasisEndorsement Basis = "endorsement"
	// BasisGrant 许可依据：人工预授予的许可命中放行。
	BasisGrant Basis = "grant"
	// BasisDuplicate 同会话重复确认 ID 的防御性放行。
	BasisDuplicate Basis = "duplicate"
	// BasisCanceled 运行取消或超时的结算拒绝。
	BasisCanceled Basis = "canceled"
	// BasisPersistFailed 确认请求持久化失败的结算拒绝。
	BasisPersistFailed Basis = "persist-failed"
)

// 内部依据值：处置为 confirm 进入人工确认流程，不是授权终态，
// 不产生观察事件。
const (
	basisForced   Basis = "forced"   // 任一 confirm 判定强制确认
	basisBaseline Basis = "baseline" // 基线确认
)

// ToolCall 是评估与分级看到的调用视图。
type ToolCall struct {
	SessionID  string
	ToolCallID string
	ToolName   string
	Arguments  json.RawMessage
}

// PolicySource 是外部裁决系统（如风控）在库中的接入点，每次工具调用
// 被同步咨询。咨询出错时网关按 confirm 判定处理并记录日志（fail-safe：
// 裁决权威不可达时不静默放行）；业务需要 fail-open 时，可在自身实现内
// 吞掉错误并返回 abstain。咨询位于工具调用的同步关键路径，源应低延迟
// 或由业务侧缓存。
type PolicySource interface {
	Consult(ctx context.Context, call ToolCall) (Verdict, error)
}

// NamedPolicy 是带名字的策略源：名字进入授权终态事件的 Source 字段，
// 标明生效依据来自哪个外部裁决系统；空名字视同匿名（Source 为空）。
type NamedPolicy struct {
	Name   string
	Source PolicySource
}

// PermissionEvent 是一次工具调用的授权终态：该调用的许可流程已结束，
// 结论不再变更。人工决定（Decide 结算，live 与 recovery 路径均是）
// 不产生事件——每次工具调用恰好一条授权终态：要么 PermissionEvent、
// 要么 Decide 结算，二者互斥且共同覆盖全部调用；人工决定由宿主在
// Decide 自身的路由与日志中审计。
type PermissionEvent struct {
	// Call 是发生授权的调用视图。
	Call ToolCall
	// Allowed 是终态是否放行。
	Allowed bool
	// Basis 是生效依据，取值见 Basis 词表。
	Basis Basis
	// Source 是生效的具名策略源名：Basis 为 verdict 或 endorsement
	// 时是首个生效源的名字，其余依据与匿名策略源为空。已知信息损失：
	// 策略源咨询出错按 confirm 判定处理（fail-safe），该调用若最终以
	// canceled 结算，事件不携带出错源的名字，出错细节以网关日志为准。
	Source string
}

// PermissionObserver 订阅网关产出的全部自动授权终态，供宿主外报审计。
// 网关在授权流程结束的同步路径上回调：回调应快速返回，重负载的外报
// 由宿主自行异步化。回调 ctx 经 context.WithoutCancel 处理、保证未取消
// （取消/超时终态亦然），回调内可安全执行 IO，需要超时自行派生。
// 回调无返回值：观察不改变授权结论，回调内部错误由宿主自行处理。
type PermissionObserver func(ctx context.Context, event PermissionEvent)

// disposition 是评估管线的输出处置。
type disposition int

const (
	dispositionAllow disposition = iota
	dispositionConfirm
	dispositionDeny
)

func (d disposition) String() string {
	switch d {
	case dispositionAllow:
		return "allow"
	case dispositionConfirm:
		return "confirm"
	default:
		return "deny"
	}
}

// resolveTier 解析工具层级：TierFunc 优先于静态表，静态表优先于默认层级。
func (g *Gateway) resolveTier(ctx context.Context, call ToolCall) Tier {
	if g.opts.tierFunc != nil {
		return g.opts.tierFunc(ctx, call)
	}
	if t, ok := g.opts.tiers[call.ToolName]; ok {
		return t
	}
	return g.opts.defaultTier
}

// namedSource 是咨询链上的一次咨询单元：策略源及其名字（匿名源为空）。
type namedSource struct {
	src  PolicySource
	name string
}

// consultResult 是一次咨询的结果：判定与来源名（匿名源为空）。
type consultResult struct {
	verdict Verdict
	source  string
}

// collectVerdicts 沿咨询链逐个同步咨询（具名源在前、匿名源在后）；
// 出错的源按 confirm 判定处理并记录日志（fail-safe）。
func (g *Gateway) collectVerdicts(ctx context.Context, call ToolCall) []consultResult {
	if len(g.opts.chain) == 0 {
		return nil
	}
	consults := make([]consultResult, 0, len(g.opts.chain))
	for _, ns := range g.opts.chain {
		v, err := ns.src.Consult(ctx, call)
		if err != nil {
			g.opts.logger.Warn("policy source consult failed, escalating to confirm",
				"tool", call.ToolName, "source", ns.name, "error", err)
			v = VerdictConfirm
		}
		consults = append(consults, consultResult{verdict: v, source: ns.name})
	}
	return consults
}

// evaluate 按固定次序得出处置、生效依据与生效来源（供日志与授权终态
// 事件）：deny 绝对（层级 deny 或任一 deny 判定）→ 强制确认（任一
// confirm 判定，许可查询不会发生）→ allow 层级 → 背书（confirm 层级
// 遇任一 allow 判定）→ 许可命中 → 基线确认。
// 处置是（层级、判定集、许可表）的纯函数，与策略源顺序无关；首个
// deny / 首个 allow 的来源归属依赖咨询链次序（具名在前、匿名在后）。
func (g *Gateway) evaluate(ctx context.Context, call ToolCall) (disposition, Basis, string) {
	tier := g.resolveTier(ctx, call)
	consults := g.collectVerdicts(ctx, call)

	if tier == TierDeny {
		return dispositionDeny, BasisTier, ""
	}
	for _, c := range consults {
		if c.verdict == VerdictDeny {
			return dispositionDeny, BasisVerdict, c.source
		}
	}
	for _, c := range consults {
		if c.verdict == VerdictConfirm {
			return dispositionConfirm, basisForced, ""
		}
	}
	if tier == TierAllow {
		return dispositionAllow, BasisTier, ""
	}
	for _, c := range consults {
		if c.verdict == VerdictAllow {
			return dispositionAllow, BasisEndorsement, c.source
		}
	}
	if g.opts.grants != nil {
		uid := g.resolveUser(ctx, call.SessionID)
		if _, ok := g.opts.grants.Lookup(ctx, GrantQuery{
			UserID: uid, SessionID: call.SessionID, ToolName: call.ToolName,
		}); ok {
			return dispositionAllow, BasisGrant, ""
		}
	}
	return dispositionConfirm, basisBaseline, ""
}

// defaultDenyText 是人工拒绝时返回给模型的默认文案；含"不得重试"指令。
func defaultDenyText(call ToolCall) string {
	return fmt.Sprintf("用户已拒绝执行工具 %s，不要再次调用该工具", call.ToolName)
}

// tierDenyReason 是层级 deny 或策略 deny 判定拒绝时返回给模型的固定文案。
func tierDenyReason(toolName string) string {
	return fmt.Sprintf("工具 %s 被许可层级或策略判定禁用，不要再次调用该工具", toolName)
}

// 运行终止与持久化失败属操作性失败，不走 WithDenyText 定制，
// 文案同样包含"不得重试"指令。
const (
	cancelDenyReason  = "运行已取消或超时，工具未执行，不要再次调用该工具"
	persistDenyReason = "确认请求持久化失败，工具未执行，不要再次调用该工具"
)

// unavailableDenyReason 是恢复路径代执行时工具不可用/失败的拒绝文案。
func unavailableDenyReason(toolName, detail string) string {
	return fmt.Sprintf("工具 %s 不可用（%s），未执行，不要再次调用该工具", toolName, detail)
}
