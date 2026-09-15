package ack

import (
	"context"
	"encoding/json"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/server/agui/adapter"
	aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// PolicyFor 返回单个会话的权限策略：该会话的每次工具调用先经许可评估，
// 评估为确认的调用进入人工确认流程。供直接使用 core runner 的业务挂载；
// 经 AG-UI runner 接入的业务用 RunOptionResolver。
func (g *Gateway) PolicyFor(sessionID string) tool.PermissionPolicyFunc {
	return func(ctx context.Context, req *tool.PermissionRequest) (tool.PermissionDecision, error) {
		if req == nil || req.ToolCallID == "" {
			return tool.AllowPermission(), nil
		}
		call := ToolCall{
			SessionID:  sessionID,
			ToolCallID: req.ToolCallID,
			ToolName:   req.ToolName,
			Arguments:  append(json.RawMessage(nil), req.Arguments...),
		}
		d, basis := g.evaluate(ctx, call)
		g.opts.logger.Info("permission evaluated",
			"tool", call.ToolName, "disposition", d.String(), "basis", basis)
		switch d {
		case dispositionDeny:
			return tool.DenyPermission(tierDenyReason(call.ToolName)), nil
		case dispositionAllow:
			return tool.AllowPermission(), nil
		default:
			return g.awaitConfirmation(ctx, call)
		}
	}
}

// RunOptionResolver 把许可策略适配为 aguirunner.WithRunOptionResolver
// 需要的形式，使每次 AG-UI 运行（含恢复路径的续跑）都挂上该策略。
func (g *Gateway) RunOptionResolver() func(ctx context.Context, in *adapter.RunAgentInput) ([]agent.RunOption, error) {
	return func(_ context.Context, in *adapter.RunAgentInput) ([]agent.RunOption, error) {
		if in == nil || in.ThreadID == "" {
			return nil, nil
		}
		return []agent.RunOption{agent.WithToolPermissionPolicyFunc(g.PolicyFor(in.ThreadID))}, nil
	}
}

// awaitConfirmation 登记内存等待者并阻塞，直到 Decide 唤醒或运行上下文
// 结束（取消/超时按拒绝结算并清理指针）。
func (g *Gateway) awaitConfirmation(ctx context.Context, call ToolCall) (tool.PermissionDecision, error) {
	p := Payload{
		ConfirmationID: call.ToolCallID,
		ToolCallID:     call.ToolCallID,
		ToolName:       call.ToolName,
		Arguments:      call.Arguments,
		SessionID:      call.SessionID,
		Status:         StatusPending,
		CreatedAt:      time.Now().UnixMilli(),
	}
	w := &waiter{payload: p, decided: make(chan bool, 1)}
	if run, ok := aguirunner.RunFromContext(ctx); ok {
		w.run = run
	}

	k := waiterKey(call.SessionID, p.ConfirmationID)
	g.mu.Lock()
	if _, dup := g.waiters[k]; dup {
		g.mu.Unlock()
		// 同会话出现重复确认 ID 时，后到请求直接放行，不进入确认流程。
		g.opts.logger.Warn("duplicate confirmation id in session, allowing",
			"sessionId", call.SessionID, "confirmationId", p.ConfirmationID)
		return tool.AllowPermission(), nil
	}
	g.waiters[k] = w
	g.mu.Unlock()

	uid := g.resolveUser(ctx, call.SessionID)
	forget := func() {
		g.mu.Lock()
		if g.waiters[k] == w {
			delete(g.waiters, k)
		}
		g.mu.Unlock()
	}

	// 先持久化后阻塞：进入等待前必须成功持久化待确认指针，
	// 持久化失败按拒绝结算，工具不执行。
	if err := g.savePending(ctx, uid, call.SessionID, func(m map[string]Payload) {
		m[p.ConfirmationID] = p
	}); err != nil {
		g.opts.logger.Error("persist pending confirmation failed",
			"sessionId", call.SessionID, "confirmationId", p.ConfirmationID, "error", err)
		forget()
		return tool.DenyPermission(persistDenyReason), nil
	}
	g.opts.logger.Info("confirmation registered",
		"sessionId", call.SessionID, "confirmationId", p.ConfirmationID, "tool", call.ToolName)

	// 把确认卡片推送给前端（实时 SSE + track 持久化）；运行不存在时跳过。
	if w.run != nil {
		if err := w.run.Emit(ctx, p.CustomEvent()); err != nil {
			g.opts.logger.Warn("emit pending confirmation event failed",
				"confirmationId", p.ConfirmationID, "error", err)
		}
	}

	select {
	case approved := <-w.decided:
		if approved {
			return tool.AllowPermission(), nil
		}
		return tool.DenyPermission(g.opts.denyText(call)), nil
	case <-ctx.Done():
		// 运行被取消或超时：清理内存等待者与持久化指针后按拒绝返回。
		forget()
		_ = g.savePending(context.WithoutCancel(ctx), uid, call.SessionID, func(m map[string]Payload) {
			delete(m, p.ConfirmationID)
		})
		return tool.DenyPermission(cancelDenyReason), nil
	}
}
