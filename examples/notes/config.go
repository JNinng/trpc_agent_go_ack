package main

import (
	"os"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/session"

	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// Config 是解析完成的示例配置。
type Config struct {
	Addr           string // HTTP 监听地址
	SessionBackend string // 会话存储后端："memory" | "postgres"
	PostgresDSN    string // PostgreSQL 连接串（backend=postgres 时使用）

	LLM       string // 模型提供方："deepseek" | "mock"
	APIKey    string // 模型 API Key；为空时自动回退到脚本化 mock 模型
	BaseURL   string // OpenAI 兼容接口地址
	ModelName string // 模型名

	RiskPolicy bool   // 启用 mock 风控策略源（演示强制确认/背书/deny 三形态）
	ToolMode   string // 工具接入模式："function"（默认）| "mcp"；由 -mcp 命令标记设置

	// 以下为测试专用钩子，不从环境变量读取。
	MockScript             []acktest.Step  // 注入 mock 模型的脚本步骤
	SessionServiceOverride session.Service // 直接注入会话存储，绕过按后端构建的逻辑
}

// FromEnv 从环境变量构建配置，未设置或为空的项取默认值。
// 环境变量前缀 NOTES_。
func FromEnv() Config {
	return Config{
		Addr:           env("NOTES_ADDR", "127.0.0.1:8080"),
		SessionBackend: strings.ToLower(env("NOTES_SESSION_BACKEND", "memory")),
		PostgresDSN:    env("NOTES_PG_DSN", "postgresql://postgres:postgres@127.0.0.1:5432/ack_notes?sslmode=disable"),
		LLM:            strings.ToLower(env("NOTES_LLM", "deepseek")),
		APIKey:         env("NOTES_API_KEY", env("DEEPSEEK_API_KEY", "")),
		BaseURL:        env("NOTES_BASE_URL", "https://api.deepseek.com"),
		ModelName:      env("NOTES_MODEL", "deepseek-chat"),
		RiskPolicy:     env("NOTES_RISK_POLICY", "0") == "1",
	}
}

// DefaultMockScript 返回离线演示脚本：读取 n1（allow 层级直接执行）→
// 删除 n2（出确认卡片）→ 删除 n3（勾选 remember 后免确认直接执行）→
// 输出总结文本。完整演示许可闭环。
func DefaultMockScript() []acktest.Step {
	return []acktest.Step{
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_1", Name: "read_note", Args: `{"note_id":"n1"}`}}},
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_2", Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_3", Name: "delete_note", Args: `{"note_id":"n3"}`}}},
		{Text: "已完成：读取了 n1，删除了 n2 和 n3。"},
	}
}

// env 读取环境变量，值为空时返回默认值。
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
