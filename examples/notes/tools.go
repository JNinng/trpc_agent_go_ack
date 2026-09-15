// examples/notes 是 ack 库的端到端示例：一个笔记助手，演示层级配置
// （allow/confirm/deny）、人工确认卡片、许可 remember 免确认、
// 历史回放与服务重启恢复。前端见 ack_notes/（Vue3）。
package main

import (
	"context"
	"sync"

	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// Notepad 是内存笔记存储，同时记录执行痕迹供测试断言。
type Notepad struct {
	mu    sync.Mutex
	notes map[string]string

	Reads   []string // 真正读取过的笔记 ID
	Written []string // 真正写入过的笔记 ID
	Deleted []string // 真正删除过的笔记 ID
	Dropped bool     // drop_all_notes 是否执行过（deny 层级下应为 false）
}

// NewNotepad 创建笔记本并预置三条演示笔记。
func NewNotepad() *Notepad {
	return &Notepad{notes: map[string]string{
		"n1": "项目上线检查清单：1. 灰度发布 2. 监控告警 3. 回滚预案",
		"n2": "会议纪要（周三）：确认工具调用需要人工审批后才能执行",
		"n3": "读书清单：《设计数据密集型应用》《程序员修炼之道》",
	}}
}

// noteInput 是 read_note / delete_note 的入参。
type noteInput struct {
	NoteID string `json:"note_id"`
}

// writeInput 是 write_note 的入参。
type writeInput struct {
	NoteID  string `json:"note_id"`
	Content string `json:"content"`
}

// Tools 返回笔记本的四个函数工具，层级在 app.go 中配置。
func (n *Notepad) Tools() []tool.Tool {
	return []tool.Tool{
		function.NewFunctionTool(n.read,
			function.WithName("read_note"),
			function.WithDescription("读取一条笔记的内容。参数 note_id 为笔记 ID（如 n1、n2、n3）。"),
		),
		function.NewFunctionTool(n.write,
			function.WithName("write_note"),
			function.WithDescription("写入一条笔记（新建或覆盖）。参数 note_id 为笔记 ID，content 为内容。"),
		),
		function.NewFunctionTool(n.delete,
			function.WithName("delete_note"),
			function.WithDescription("永久删除一条笔记，不可恢复。参数 note_id 为笔记 ID（如 n1、n2、n3）。"),
		),
		function.NewFunctionTool(n.drop,
			function.WithName("drop_all_notes"),
			function.WithDescription("清空全部笔记。该工具被许可层级禁用，用于演示 deny 不可降级。"),
		),
	}
}

func (n *Notepad) read(_ context.Context, in noteInput) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	content, ok := n.notes[in.NoteID]
	if !ok {
		return map[string]any{"found": false, "note_id": in.NoteID}, nil
	}
	n.Reads = append(n.Reads, in.NoteID)
	return map[string]any{"found": true, "note_id": in.NoteID, "content": content}, nil
}

func (n *Notepad) write(_ context.Context, in writeInput) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes[in.NoteID] = in.Content
	n.Written = append(n.Written, in.NoteID)
	return map[string]any{"written": true, "note_id": in.NoteID}, nil
}

func (n *Notepad) delete(_ context.Context, in noteInput) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.notes[in.NoteID]; !ok {
		return map[string]any{"deleted": false, "note_id": in.NoteID, "reason": "笔记不存在"}, nil
	}
	delete(n.notes, in.NoteID)
	n.Deleted = append(n.Deleted, in.NoteID)
	return map[string]any{"deleted": true, "note_id": in.NoteID}, nil
}

func (n *Notepad) drop(context.Context, struct{}) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Dropped = true
	return map[string]any{"dropped": true}, nil
}
