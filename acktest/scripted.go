// Package acktest 提供无真实 LLM 时的确定性测试夹具：脚本化模型
// （model.Model 实现）与假续跑 runner，供库自身的测试与业务侧的
// 确认流集成测试使用。
package acktest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// ToolCallSpec 描述脚本化模型发出的一次工具调用。
type ToolCallSpec struct {
	ID   string
	Name string
	Args string // JSON 格式的参数
}

// Step 是一步脚本化的模型响应：文本、工具调用，或两者兼有。
type Step struct {
	Text      string
	ToolCalls []ToolCallSpec
}

// ScriptedModel 是按固定脚本回放的 model.Model 实现：轮次索引等于
// 请求历史中 RoleTool 消息的数量，因此可以跨多轮运行正确推进。
// 脚本耗尽后重复最后一步，保证长对话不会越界。
type ScriptedModel struct {
	name  string // 模型名，Info 时返回
	steps []Step // 脚本步骤，按轮次依次消费
}

// NewScriptedModel 创建脚本化模型；未提供步骤时使用一条占位文本。
func NewScriptedModel(name string, steps ...Step) *ScriptedModel {
	if len(steps) == 0 {
		steps = []Step{{Text: "（脚本化模型：没有可执行的步骤）"}}
	}
	return &ScriptedModel{name: name, steps: steps}
}

// Info 实现 model.Model，返回模型名。
func (m *ScriptedModel) Info() model.Info {
	return model.Info{Name: m.name}
}

// GenerateContent 实现 model.Model，按轮次索引选取脚本步骤。
func (m *ScriptedModel) GenerateContent(_ context.Context, req *model.Request) (<-chan *model.Response, error) {
	if req == nil {
		return nil, &model.ResponseError{Type: "invalid_request_error", Message: "acktest: request is nil"}
	}
	turn := 0
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			turn++
		}
	}
	idx := turn
	if idx >= len(m.steps) {
		idx = len(m.steps) - 1
	}
	step := m.steps[idx]

	msg := model.Message{Role: model.RoleAssistant, Content: step.Text}
	finishReason := "stop"
	if len(step.ToolCalls) > 0 {
		finishReason = "tool_calls"
		for _, tc := range step.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: model.FunctionDefinitionParam{
					Name:      tc.Name,
					Arguments: []byte(tc.Args),
				},
			})
		}
	}

	resp := &model.Response{
		ID:      "acktest-" + uuid.NewString(),
		Object:  model.ObjectTypeChatCompletion,
		Created: time.Now().Unix(),
		Choices: []model.Choice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Done: true,
	}
	ch := make(chan *model.Response, 1)
	ch <- resp
	close(ch)
	return ch, nil
}
