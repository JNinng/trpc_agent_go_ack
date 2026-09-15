package ack

import "errors"

var (
	// ErrNotFound 表示确认或会话不存在；含已决定（或已取消）后的
	// 重复决定与并发决定中的后到者。
	ErrNotFound = errors.New("ack: confirmation not found")

	// ErrNotBound 表示恢复路径需要续跑 runner 但未 Bind。
	ErrNotBound = errors.New("ack: resume runner not bound")

	// ErrRunActive 表示续跑时会话上仍有运行进行；对框架运行冲突
	// 错误包装，errors.Is 可判。
	ErrRunActive = errors.New("ack: run active on session")
)
