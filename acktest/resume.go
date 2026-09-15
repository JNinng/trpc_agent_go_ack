package acktest

import (
	"context"
	"fmt"
	"sync"

	aguievents "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"

	"trpc.group/trpc-go/trpc-agent-go/server/agui/adapter"
)

// ResumeCall 是一次续跑调用的关键参数快照。
type ResumeCall struct {
	ThreadID   string // 续跑的会话 ID
	RunID      string // 续跑运行的 ID（resume- 前缀）
	ToolCallID string // 补投的工具调用 ID
	ToolName   string // 补投的工具名
	Content    string // 补投的工具结果内容
}

// ResumeRecorder 是假续跑 runner：记录 Run 调用，返回立即关闭的
// 事件通道，不真正运行 AG-UI。可用 FailWith 注入错误以演练失败路径。
type ResumeRecorder struct {
	mu   sync.Mutex
	call []ResumeCall
	fail error
}

// NewResumeRecorder 创建空的续跑记录器。
func NewResumeRecorder() *ResumeRecorder {
	return &ResumeRecorder{}
}

// FailWith 设置后续 Run 调用返回的错误（传 nil 清除）。
func (r *ResumeRecorder) FailWith(err error) {
	r.mu.Lock()
	r.fail = err
	r.mu.Unlock()
}

// Run 实现 ack.ResumeRunner。
func (r *ResumeRecorder) Run(_ context.Context, input *adapter.RunAgentInput) (<-chan aguievents.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	call := ResumeCall{ThreadID: input.ThreadID, RunID: input.RunID}
	if len(input.Messages) > 0 {
		call.ToolCallID = input.Messages[0].ToolCallID
		call.ToolName = input.Messages[0].Name
		call.Content = fmt.Sprint(input.Messages[0].Content)
	}
	r.call = append(r.call, call)
	ch := make(chan aguievents.Event)
	close(ch)
	return ch, nil
}

// Calls 返回已记录的续跑调用快照。
func (r *ResumeRecorder) Calls() []ResumeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ResumeCall(nil), r.call...)
}
