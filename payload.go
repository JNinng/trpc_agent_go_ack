package ack

import (
	"encoding/json"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
)

// 以下常量是 wire 与存储的兼容性契约：升级只增不改，否则遗留待确认
// 指针失联、前端事件配对失效。
const (
	// EventName 是携带确认 Payload 的 AG-UI 自定义事件名。
	EventName = "tool_confirmation"

	// StateKey 是待确认表在 Session.State 中的键，
	// 让"等待确认"状态可以在服务重启后存活。
	StateKey = "__pending_tool_confirmation__"

	// StatusPending 表示确认正等待用户决定。
	StatusPending = "pending"
	// StatusApproved 表示用户已同意该确认。
	StatusApproved = "approved"
	// StatusDenied 表示用户已拒绝该确认。
	StatusDenied = "denied"
)

// Payload 是一次工具确认的完整描述：随 AG-UI CustomEvent 下发前端，
// 同时以待确认指针的形式持久化在 Session.State 中。
type Payload struct {
	ConfirmationID string          `json:"confirmationId"`      // 确认 ID（即工具调用 ID）
	ToolCallID     string          `json:"toolCallId"`          // 关联的工具调用 ID
	ToolName       string          `json:"toolName"`            // 工具名
	Arguments      json.RawMessage `json:"arguments,omitempty"` // 工具入参（JSON）
	SessionID      string          `json:"sessionId"`           // 所属会话 ID
	Status         string          `json:"status"`              // pending | approved | denied
	DecidedBy      string          `json:"decidedBy,omitempty"` // 做出决定的用户
	CreatedAt      int64           `json:"createdAt"`           // 创建时间（毫秒时间戳）
}

// CustomEvent 把 Payload 包装为 AG-UI 自定义事件。
func (p Payload) CustomEvent() *aguievents.CustomEvent {
	return aguievents.NewCustomEvent(EventName, aguievents.WithValue(p))
}

// decodePendingMap 解析存储在 Session.State 中的待确认表；空数据返回空表。
func decodePendingMap(raw []byte) (map[string]Payload, error) {
	m := map[string]Payload{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// encodePendingMap 把待确认表序列化后写入 Session.State；
// 序列化失败时退回空表 "{}"，避免写入损坏数据。
func encodePendingMap(m map[string]Payload) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// deniedContent 构造拒绝时模型看到的工具结果内容。它刻意对齐框架
// tool.PermissionResult 的 JSON 形状，使模型对直接路径拒绝（权限策略
// 拒绝）与恢复路径拒绝（续跑运行中的拒绝）的理解保持一致。
func deniedContent(toolName, reason string) string {
	b, _ := json.Marshal(map[string]string{
		"status": "denied",
		"tool":   toolName,
		"reason": reason,
	})
	return string(b)
}
