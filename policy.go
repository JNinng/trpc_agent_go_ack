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
			// 防御性放行：无调用标识、非真实工具调用，不在授权终态
			// 观察契约内。
			return tool.AllowPermission(), nil
		}
		call := ToolCall{
			SessionID:  sessionID,
			ToolCallID: req.ToolCallID,
			ToolName:   req.ToolName,
			Arguments:  append(json.RawMessage(nil), req.Arguments...),
		}
		d, basis, source := g.evaluate(ctx, call)
		g.opts.logger.Info("permission evaluated",
			"tool", call.ToolName, "disposition", d.String(), "basis", string(basis))
		switch d {
		case dispositionDeny:
			g.observe(ctx, call, false, basis, source)
			return tool.DenyPermission(tierDenyReason(call.ToolName)), nil
		case dispositionAllow:
			g.observe(ctx, call, true, basis, source)
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
		g.observe(ctx, call, true, BasisDuplicate, "")
		return tool.AllowPermission(), nil
	}
	g.waiters[k] = w
	g.mu.Unlock()

	uid := g.resolveUser(ctx, call.SessionID)

	// 先持久化后阻塞：进入等待前必须成功持久化待确认指针，
	// 持久化失败按拒绝结算，工具不执行。
	if err := g.savePending(ctx, uid, call.SessionID, func(m map[string]Payload) {
		m[p.ConfirmationID] = p
	}); err != nil {
		g.opts.logger.Error("persist pending confirmation failed",
			"sessionId", call.SessionID, "confirmationId", p.ConfirmationID, "error", err)
		return g.settleOperational(ctx, uid, call, w, BasisPersistFailed, persistDenyReason)
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
		// 人工决定：不发观察事件，归宿主 Decide 路由审计。
		return g.decisionToResult(call, approved)
	case <-ctx.Done():
		// 运行被取消或超时：清理内存等待者与持久化指针后按拒绝返回。
		return g.settleOperational(ctx, uid, call, w, BasisCanceled, cancelDenyReason)
	}
}

// settleOperational 结算一次操作性拒绝终态（持久化失败 / 取消超时）。
// 经结算权认领与 Decide 互斥；认领成功且登记表中的等待者仍是本调用的
// （身份校验），清理持久化指针并发出观察事件；等待者已被人工决定认领
// 并结算时按该决定返回、不发事件——决定归宿主 Decide 路由审计。
// 认领失败（结算权被占）时在「决定写入」与「退避重试认领」间竞速：
// 占用者可能是认领了本等待者的 settleLive（必然写入 decided），也可能
// 是同 ID 前任调用的操作性结算或恢复路径（永不写入本等待者）。
func (g *Gateway) settleOperational(ctx context.Context, uid string, call ToolCall, w *waiter, basis Basis, denyReason string) (tool.PermissionDecision, error) {
	sid, cid := call.SessionID, w.payload.ConfirmationID
	var current *waiter
	for {
		var ok bool
		if current, ok = g.beginSettle(sid, cid, w); ok {
			break
		}
		select {
		case approved := <-w.decided:
			return g.decisionToResult(call, approved)
		case <-time.After(time.Millisecond):
		}
	}
	defer g.endSettle(sid, cid)
	if current != w {
		// 等待者已被一次已完成的人工决定认领并结算（其发送先于其
		// 释放结算权）：按该决定返回，不发操作性事件。
		return g.decisionToResult(call, <-w.decided)
	}
	if err := g.savePending(context.WithoutCancel(ctx), uid, sid, func(m map[string]Payload) {
		delete(m, cid)
	}); err != nil {
		g.opts.logger.Error("clear pending confirmation failed",
			"sessionId", sid, "confirmationId", cid, "error", err)
	}
	g.observe(ctx, call, false, basis, "")
	return tool.DenyPermission(denyReason), nil
}

// decisionToResult 把人工决定转换为许可决定：同意放行，拒绝用定制文案。
func (g *Gateway) decisionToResult(call ToolCall, approved bool) (tool.PermissionDecision, error) {
	if approved {
		return tool.AllowPermission(), nil
	}
	return tool.DenyPermission(g.opts.denyText(call)), nil
}

// observe 在授权终态处回调观察者；缺省 nil 零行为。回调 ctx 经
// context.WithoutCancel 处理：取消/超时终态下回调仍可执行 IO。
func (g *Gateway) observe(ctx context.Context, call ToolCall, allowed bool, basis Basis, source string) {
	if g.opts.observer == nil {
		return
	}
	g.opts.observer(context.WithoutCancel(ctx), PermissionEvent{
		Call:    call,
		Allowed: allowed,
		Basis:   basis,
		Source:  source,
	})
}
