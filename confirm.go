package ack

import (
	"context"
	"fmt"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"
)

// 决定实际生效的路径。
const (
	// PathLive 直接路径：发出确认的运行仍存活，决定唤醒阻塞的运行。
	PathLive = "live"
	// PathRecovery 恢复路径：运行已不存在，网关代执行/合成结果并续跑。
	PathRecovery = "recovery"
)

// Decision 是一次人工决定的完整参数；以结构体承载演进空间，
// 后续以增字段方式扩展。
type Decision struct {
	SessionID      string // 会话 ID
	ConfirmationID string // 确认 ID
	Approved       bool   // true=同意，false=拒绝
	DecidedBy      string // 决定人；留空取网关身份
	Remember       Scope  // 空=不生成许可；同意时按作用域写入许可源
}

// DecideResult 是 Decide 的结果。
type DecideResult struct {
	Path string // PathLive | PathRecovery
}

// Decide 结算一次人工决定，返回实际生效的路径（PathLive / PathRecovery）。
// ctx 约束持久化与续跑的启动，不等待续跑完成。
// 同一确认的并发决定先到者生效，后到者返回 ErrNotFound。
func (g *Gateway) Decide(ctx context.Context, d Decision) (DecideResult, error) {
	uid := g.resolveUser(ctx, d.SessionID)
	decidedBy := d.DecidedBy
	if decidedBy == "" {
		decidedBy = uid
	}

	w, ok := g.beginSettle(d.SessionID, d.ConfirmationID, nil)
	if !ok {
		return DecideResult{}, ErrNotFound
	}
	defer g.endSettle(d.SessionID, d.ConfirmationID)

	if w != nil {
		return g.settleLive(ctx, uid, decidedBy, d, w)
	}
	return g.settleRecovered(ctx, uid, decidedBy, d)
}

// settleLive 结算存活的等待者：清除指针 → 推送终态确认事件 → 唤醒运行。
// 同意则工具在原运行内继续执行，事件仍走原 SSE/follow 流；拒绝则框架
// 向模型返回 denied 结果，工具不执行。
func (g *Gateway) settleLive(ctx context.Context, uid, decidedBy string, d Decision, w *waiter) (DecideResult, error) {
	p := w.payload
	p.Status = StatusDenied
	if d.Approved {
		p.Status = StatusApproved
	}
	p.DecidedBy = decidedBy

	if err := g.savePending(ctx, uid, d.SessionID, func(m map[string]Payload) {
		delete(m, d.ConfirmationID)
	}); err != nil {
		g.opts.logger.Error("clear pending confirmation failed",
			"sessionId", d.SessionID, "confirmationId", d.ConfirmationID, "error", err)
	}
	g.rememberGrant(ctx, uid, decidedBy, d, p.ToolName)
	if w.run != nil {
		if err := w.run.Emit(ctx, p.CustomEvent()); err != nil {
			g.opts.logger.Warn("emit decision confirmation event failed",
				"confirmationId", d.ConfirmationID, "error", err)
		}
	}
	w.decided <- d.Approved
	g.opts.logger.Info("confirmation decided",
		"sessionId", d.SessionID, "confirmationId", d.ConfirmationID,
		"approved", d.Approved, "path", PathLive)
	return DecideResult{Path: PathLive}, nil
}

// PendingItem 是待确认列表项；Live 标记阻塞中的运行是否仍存活
// （false 表示已成孤儿，例如服务重启之后）。
type PendingItem struct {
	Payload
	Live bool `json:"live"`
}

// Pending 列出会话中等待决定的确认项：合并内存中的存活等待者与
// Session.State 持久化的记录（含重启后的孤儿）。
func (g *Gateway) Pending(ctx context.Context, sessionID string) ([]PendingItem, error) {
	uid := g.resolveUser(ctx, sessionID)

	g.mu.Lock()
	live := make(map[string]struct{}, len(g.waiters))
	for _, w := range g.waiters {
		if w.payload.SessionID == sessionID {
			live[w.payload.ConfirmationID] = struct{}{}
		}
	}
	g.mu.Unlock()

	sess, err := g.sessions.GetSession(ctx, g.key(uid, sessionID))
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return []PendingItem{}, nil
	}
	raw, _ := sess.GetState(StateKey)
	m, err := decodePendingMap(raw)
	if err != nil {
		return nil, err
	}
	items := make([]PendingItem, 0, len(m))
	for id, p := range m {
		_, isLive := live[id]
		items = append(items, PendingItem{Payload: p, Live: isLive})
	}
	return items, nil
}

// rememberGrant 在人工同意且显式指定 Remember 时向许可源写入对应
// 作用域的许可。写入失败不回退本次许可，仅记录日志；未配置许可源时
// Remember 被忽略并记录日志。
func (g *Gateway) rememberGrant(ctx context.Context, uid, decidedBy string, d Decision, toolName string) {
	if !d.Approved || d.Remember == "" {
		return
	}
	if d.Remember != ScopeSession && d.Remember != ScopeGlobal {
		g.opts.logger.Warn("unknown remember scope, ignoring", "scope", string(d.Remember))
		return
	}
	if g.opts.grants == nil {
		g.opts.logger.Warn("remember requested but no grant source configured, ignoring",
			"confirmationId", d.ConfirmationID)
		return
	}
	gr := Grant{
		UserID:    uid,
		ToolName:  toolName,
		CreatedBy: decidedBy,
		CreatedAt: time.Now().UnixMilli(),
	}
	if d.Remember == ScopeSession {
		gr.SessionID = d.SessionID
	}
	if err := g.opts.grants.Save(ctx, gr); err != nil {
		g.opts.logger.Error("save grant failed", "tool", toolName, "error", err)
	}
}

// beginSettle 原子地认领一次确认的结算权：登记 settling 防止并发结算
// 双重结算，并按身份摘除等待者——self 为 nil 时摘除当前登记者（Decide
// 结算任意存活等待者）；self 非 nil 时仅当登记者正是该等待者才摘除
// （操作性结算的身份校验，防止误摘同 ID 后到的新等待者）。返回被摘除
// 的等待者（可能为 nil）；ok=false 表示结算权被占，后到者生效为
// ErrNotFound。
func (g *Gateway) beginSettle(sessionID, confirmationID string, self *waiter) (*waiter, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := waiterKey(sessionID, confirmationID)
	if _, busy := g.settling[k]; busy {
		return nil, false
	}
	g.settling[k] = struct{}{}
	current := g.waiters[k]
	if self == nil || current == self {
		delete(g.waiters, k)
	}
	return current, true
}

// endSettle 释放结算权。
func (g *Gateway) endSettle(sessionID, confirmationID string) {
	g.mu.Lock()
	delete(g.settling, waiterKey(sessionID, confirmationID))
	g.mu.Unlock()
}

// key 构造定位会话的存储键。
func (g *Gateway) key(uid, sessionID string) session.Key {
	return session.Key{AppName: g.opts.app, UserID: uid, SessionID: sessionID}
}

// savePending 在网关锁的保护下对待确认表做"读取 → 修改 → 写回"，
// 单进程下串行一致。
func (g *Gateway) savePending(ctx context.Context, uid, sessionID string, mutate func(map[string]Payload)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	sess, err := g.sessions.GetSession(ctx, g.key(uid, sessionID))
	if err != nil {
		return err
	}
	if sess == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	raw, _ := sess.GetState(StateKey)
	m, err := decodePendingMap(raw)
	if err != nil {
		return err
	}
	mutate(m)
	return g.sessions.UpdateSessionState(ctx, g.key(uid, sessionID), session.StateMap{
		StateKey: encodePendingMap(m),
	})
}
