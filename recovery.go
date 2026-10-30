package ack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"

	"trpc.group/trpc-go/trpc-agent-go/server/agui/adapter"
	aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// settleRecovered 结算孤儿确认（决定到达时无存活等待者，网关以待确认
// 指针为准判定）：清除指针 → 从会话历史还原原始工具调用 → 同意则在
// 服务端代执行工具、拒绝则合成 denied 结果 → 以 RoleTool 消息拉起
// 一次续跑运行，事件落入 track，前端经 /history follow 流读取。
//
// 历史中找不回原始调用、或工具在裁决时刻不可注册时，降级为拒绝续跑
// （安全优先，不执行未知调用）。这是基于会话历史的软恢复（模型重读
// 历史后继续），非执行现场无损恢复。
func (g *Gateway) settleRecovered(ctx context.Context, uid, decidedBy string, d Decision) (DecideResult, error) {
	g.mu.Lock()
	resume := g.resume
	g.mu.Unlock()

	sess, err := g.sessions.GetSession(ctx, g.key(uid, d.SessionID))
	if err != nil {
		return DecideResult{}, fmt.Errorf("ack: load session for orphan confirmation: %w", err)
	}
	if sess == nil {
		// postgres 等后端对不存在的会话返回 (nil, nil)。
		return DecideResult{}, ErrNotFound
	}
	raw, _ := sess.GetState(StateKey)
	m, err := decodePendingMap(raw)
	if err != nil {
		return DecideResult{}, fmt.Errorf("ack: decode pending confirmations: %w", err)
	}
	if _, ok := m[d.ConfirmationID]; !ok {
		return DecideResult{}, ErrNotFound
	}
	if resume == nil {
		return DecideResult{}, ErrNotBound
	}
	delete(m, d.ConfirmationID)
	if err := g.sessions.UpdateSessionState(ctx, g.key(uid, d.SessionID), session.StateMap{
		StateKey: encodePendingMap(m),
	}); err != nil {
		return DecideResult{}, fmt.Errorf("ack: clear orphan pending: %w", err)
	}

	toolName, args, ferr := findToolCall(sess, d.ConfirmationID)
	approved := d.Approved
	if ferr != nil {
		g.opts.logger.Warn("recover original tool call failed, resuming with denial",
			"confirmationId", d.ConfirmationID, "error", ferr)
		toolName, args, approved = "unknown", nil, false
	}
	content := g.materializeToolResult(ctx, toolName, args, approved)
	if ferr == nil {
		g.rememberGrant(ctx, uid, decidedBy, d, toolName)
	}

	input := &adapter.RunAgentInput{
		ThreadID: d.SessionID,
		RunID:    "resume-" + uuid.NewString(),
		Messages: []aguitypes.Message{{
			ID:         "confirm-" + d.ConfirmationID,
			Role:       aguitypes.RoleTool,
			ToolCallID: d.ConfirmationID,
			Name:       toolName,
			Content:    content,
		}},
	}
	ch, err := resume.Run(ctx, input)
	if err != nil {
		if errors.Is(err, aguirunner.ErrRunAlreadyExists) {
			// 会话上仍有运行进行（409 场景）。
			return DecideResult{}, fmt.Errorf("%w: %w", ErrRunActive, err)
		}
		return DecideResult{}, fmt.Errorf("ack: resume run: %w", err)
	}
	g.opts.logger.Info("orphan confirmation resumed",
		"sessionId", d.SessionID, "confirmationId", d.ConfirmationID,
		"approved", d.Approved, "degraded", ferr != nil)
	// 事件由 AG-UI runner 自行持久化到 track；这里排空通道只是防止发送
	// 侧阻塞。前端通过 /history follow 流读取这些事件。
	go func() {
		for range ch {
		}
	}()
	return DecideResult{Path: PathRecovery}, nil
}

// materializeToolResult 在服务端直接执行工具（同意时）或构造拒绝结果，
// 输出模型期望的工具消息内容。仅在恢复路径使用。
func (g *Gateway) materializeToolResult(ctx context.Context, toolName string, args []byte, approved bool) string {
	if !approved {
		return deniedContent(toolName, g.opts.denyTexts.HumanDenied(ToolCall{ToolName: toolName}))
	}
	if g.opts.tools == nil {
		return deniedContent(toolName, g.opts.denyTexts.Unavailable(toolName, "no tool resolver configured"))
	}
	t, ok := g.opts.tools(ctx)[toolName]
	if !ok {
		return deniedContent(toolName, g.opts.denyTexts.Unavailable(toolName, "tool not registered at decision time"))
	}
	res, err := t.Call(ctx, args)
	if err != nil {
		return deniedContent(toolName, g.opts.denyTexts.Unavailable(toolName, "call failed: "+err.Error()))
	}
	b, err := json.Marshal(res)
	if err != nil {
		return deniedContent(toolName, g.opts.denyTexts.Unavailable(toolName, "failed to marshal result: "+err.Error()))
	}
	return string(b)
}

// findToolCall 从会话事件历史中找回原始工具调用的工具名与参数，
// 供恢复路径在服务端重放执行。
func findToolCall(sess *session.Session, toolCallID string) (string, []byte, error) {
	for i := range sess.Events {
		ev := &sess.Events[i]
		if ev.Response == nil {
			continue
		}
		for _, choice := range ev.Response.Choices {
			for _, tc := range choice.Message.ToolCalls {
				if tc.ID == toolCallID {
					return tc.Function.Name, tc.Function.Arguments, nil
				}
			}
		}
	}
	return "", nil, fmt.Errorf("tool call %s not found in session history", toolCallID)
}
