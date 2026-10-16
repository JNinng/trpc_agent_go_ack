// notes 示例后端入口：基于 trpc-agent-go 的 AG-UI SSE 服务与 ack
// 许可网关。运行方式见仓库 README。
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
)

// main 解析配置、组装应用并启动 HTTP 服务，监听 SIGINT 优雅退出。
// LLM 为 mock 或未配置 API Key 时启用默认脚本，保证离线可演示；
// -mcp 标记自动启动 MCP server 并以 MCP 工具模式对接。
func main() {
	mcpMode := flag.Bool("mcp", false, "自动启动 MCP server 并对接（MCP 工具模式；默认 function 模式）")
	flag.Parse()

	cfg := FromEnv()
	if *mcpMode {
		cfg.ToolMode = "mcp"
	}
	if cfg.LLM == "mock" || cfg.APIKey == "" {
		cfg.MockScript = DefaultMockScript()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	a, err := New(ctx, cfg)
	if err != nil {
		slog.Error("start failed", "error", err)
		os.Exit(1)
	}
	defer a.Close()

	slog.Info("notes example listening",
		"addr", cfg.Addr, "backend", cfg.SessionBackend, "llm", cfg.LLM,
		"riskPolicy", cfg.RiskPolicy, "toolMode", cfg.ToolMode)
	if a.MCPURL != "" {
		slog.Info("mcp tool server", "url", a.MCPURL)
	}
	slog.Info("endpoints: / (web) | POST /agui | POST /agui/history | POST /agui/cancel | " +
		"POST /ack/confirmation | GET /ack/confirmations | GET /api/sessions")

	srv := &http.Server{Addr: cfg.Addr, Handler: a.Handler}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		// http.ErrServerClosed 是主动关闭的正常返回，不算错误。
		if err != nil && err != http.ErrServerClosed {
			slog.Error("serve failed", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		_ = srv.Close()
	}
}
