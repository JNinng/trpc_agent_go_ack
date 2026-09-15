package ack

import (
	"context"
	"sync"
)

// Scope 是许可的作用域。
type Scope string

const (
	// ScopeSession 会话级：绑定单个会话。
	ScopeSession Scope = "session"
	// ScopeGlobal 全局级：对该用户的全部会话生效。
	ScopeGlobal Scope = "global"
)

// Grant 是用户对某工具的预先授权，可满足 confirm 层级的确认要求；
// 强制确认（策略源 confirm 判定触发）不可被许可满足。
type Grant struct {
	UserID    string // 归属用户
	SessionID string // 非空即会话级；空即全局级
	ToolName  string
	CreatedBy string // 决定来源（人工决定的 DecidedBy）
	CreatedAt int64  // 毫秒时间戳
}

// GrantQuery 是许可查询条件。
type GrantQuery struct {
	UserID    string
	SessionID string
	ToolName  string
}

// GrantSource 是许可存储的接入点，由业务实现（如用户设置系统）。
// 生命周期归业务管理（列出/吊销/过期）；库仅在许可评估时读取、在
// Remember 决定时写入。Lookup 须按"会话级先于全局级"的次序命中。
type GrantSource interface {
	Lookup(ctx context.Context, q GrantQuery) (Grant, bool)
	Save(ctx context.Context, g Grant) error
}

// MemoryGrants 返回线程安全的内存许可源，供测试与示例使用；
// 进程内生效，重启即失。
func MemoryGrants() GrantSource {
	return &memoryGrants{grants: map[grantKey]Grant{}}
}

// grantKey 以（用户、会话、工具）唯一定位一条许可；会话为空表示全局级。
type grantKey struct {
	user, session, tool string
}

type memoryGrants struct {
	mu     sync.RWMutex
	grants map[grantKey]Grant
}

// Lookup 先查会话级条目，未命中再查全局级条目。
func (m *memoryGrants) Lookup(_ context.Context, q GrantQuery) (Grant, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if q.SessionID != "" {
		if g, ok := m.grants[grantKey{q.UserID, q.SessionID, q.ToolName}]; ok {
			return g, true
		}
	}
	g, ok := m.grants[grantKey{q.UserID, "", q.ToolName}]
	return g, ok
}

func (m *memoryGrants) Save(_ context.Context, g Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[grantKey{g.UserID, g.SessionID, g.ToolName}] = g
	return nil
}
