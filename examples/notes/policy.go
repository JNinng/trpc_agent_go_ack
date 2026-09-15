package main

import (
	"context"
	"strings"

	"github.com/jninng/trpc_agent_go_ack"
)

// riskPolicy 是演示用的 mock 风控策略源，展示策略判定的三形态：
//   - 强制确认：read_note 读取受保护笔记 n1 时升级为人工确认
//     （allow 层级也被策略升压，且不可被许可满足）；
//   - 背书：write_note 被风控放行（confirm 层级降级为直接执行）；
//   - 拒绝：delete_note 删除受保护笔记 n1 时直接拒绝（不可降级）。
type riskPolicy struct{}

// Consult 实现 ack.PolicySource。
func (riskPolicy) Consult(_ context.Context, call ack.ToolCall) (ack.Verdict, error) {
	switch call.ToolName {
	case "read_note":
		if strings.Contains(string(call.Arguments), `"n1"`) {
			return ack.VerdictConfirm, nil
		}
	case "write_note":
		return ack.VerdictAllow, nil
	case "delete_note":
		if strings.Contains(string(call.Arguments), `"n1"`) {
			return ack.VerdictDeny, nil
		}
	}
	return ack.VerdictAbstain, nil
}
