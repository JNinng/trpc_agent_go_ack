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

// collectVerdicts 逐个同步咨询策略源；出错的源按 confirm 判定处理
// 并记录日志（fail-safe）。
func (g *Gateway) collectVerdicts(ctx context.Context, call ToolCall) []Verdict {
	if len(g.opts.policies) == 0 {
		return nil
	}
	verdicts := make([]Verdict, 0, len(g.opts.policies))
	for _, src := range g.opts.policies {
		v, err := src.Consult(ctx, call)
		if err != nil {
			g.opts.logger.Warn("policy source consult failed, escalating to confirm",
				"tool", call.ToolName, "error", err)
			v = VerdictConfirm
		}
		verdicts = append(verdicts, v)
	}
	return verdicts
}

// evaluate 按固定次序得出处置与命中依据（供日志）：
// deny 绝对（层级 deny 或任一 deny 判定）→ 强制确认（任一 confirm
// 判定，许可查询不会发生）→ allow 层级 → 背书（confirm 层级遇任一
// allow 判定）→ 许可命中 → 基线确认。
// 处置是（层级、判定集、许可表）的纯函数，与策略源顺序无关。
func (g *Gateway) evaluate(ctx context.Context, call ToolCall) (disposition, string) {
	tier := g.resolveTier(ctx, call)
	verdicts := g.collectVerdicts(ctx, call)

	if tier == TierDeny {
		return dispositionDeny, "tier"
	}
	for _, v := range verdicts {
		if v == VerdictDeny {
			return dispositionDeny, "verdict"
		}
	}
	for _, v := range verdicts {
		if v == VerdictConfirm {
			return dispositionConfirm, "forced"
		}
	}
	if tier == TierAllow {
		return dispositionAllow, "tier"
	}
	for _, v := range verdicts {
		if v == VerdictAllow {
			return dispositionAllow, "endorsement"
		}
	}
	if g.opts.grants != nil {
		uid := g.resolveUser(ctx, call.SessionID)
		if _, ok := g.opts.grants.Lookup(ctx, GrantQuery{
			UserID: uid, SessionID: call.SessionID, ToolName: call.ToolName,
		}); ok {
			return dispositionAllow, "grant"
		}
	}
	return dispositionConfirm, "baseline"
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
