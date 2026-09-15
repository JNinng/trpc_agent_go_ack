package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" database/sql 驱动

	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/server/agui"
	aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/session/postgres"

	"github.com/jninng/trpc_agent_go_ack"
	"github.com/jninng/trpc_agent_go_ack/ackhttp"
	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// 示例使用固定身份：单用户、无鉴权。多用户场景用 ack.WithUserFunc
// 从请求上下文取鉴权结果。
const (
	AppName = "notes"
	UserID  = "user"
)

// instruction 是注入给 Agent 的系统提示词。
const instruction = `你是一个笔记助手。可用工具：
- read_note：读取一条笔记（参数 note_id，如 n1、n2、n3）
- write_note：写入一条笔记（参数 note_id、content）
- delete_note：永久删除一条笔记（参数 note_id，不可恢复）
- drop_all_notes：清空全部笔记（被许可层级禁用，永远无法执行）

权限规则：
1. 部分工具在真正执行前需要用户在前端确认，确认通过后才会执行。
2. 如果工具结果中 status 为 "denied"，表示该操作被拒绝：不要重试同一个工具，直接向用户说明操作被拒绝。
3. 工具执行完成后，用一句中文向用户汇报结果。`

// App 是组装完成的示例后端，Handler 可直接挂到 http.Server。
type App struct {
	Config   Config          // 启动配置（Close 依据它判断资源归属）
	Notepad  *Notepad        // 内存笔记本：演示用工具集
	Gateway  *ack.Gateway    // 许可网关（live 唤醒 / recovery 续跑）
	Sessions session.Service // 会话存储（内存或 PostgreSQL）
	Handler  http.Handler    // 完整 HTTP 路由（前端 + /agui/* + /ack/* + /api/*）

	coreRunner runner.Runner // trpc-agent-go 核心 runner
}

// New 按配置组装整个应用：会话存储 → 模型（真实模型或脚本化 mock）→
// Agent → core runner → 许可网关 → AG-UI runner/server → HTTP 路由。
func New(ctx context.Context, cfg Config) (*App, error) {
	a := &App{Config: cfg, Notepad: NewNotepad()}

	svc, err := a.buildSessionService(ctx)
	if err != nil {
		return nil, err
	}
	a.Sessions = svc

	var llm model.Model
	if cfg.LLM == "mock" || cfg.APIKey == "" {
		if cfg.LLM != "mock" {
			slog.Warn("no API key configured, falling back to the scripted mock model")
		}
		llm = acktest.NewScriptedModel("notes-mock", cfg.MockScript...)
	} else {
		llm = openai.New(cfg.ModelName,
			openai.WithBaseURL(cfg.BaseURL),
			openai.WithAPIKey(cfg.APIKey),
		)
	}

	ag := llmagent.New("notes-agent",
		llmagent.WithModel(llm),
		llmagent.WithGlobalInstruction(instruction),
		llmagent.WithTools(a.Notepad.Tools()),
		llmagent.WithMaxToolIterations(8), // 拒绝后的重试兜底
	)

	a.coreRunner = runner.NewRunner(AppName, ag, runner.WithSessionService(svc))

	gwOpts := []ack.Option{
		ack.WithApp(AppName), // 须与 runner 的 AppName 一致
		ack.WithUser(UserID),
		ack.WithTiers(map[string]ack.Tier{ // 未分级工具默认 allow
			"read_note":      ack.TierAllow,
			"write_note":     ack.TierConfirm,
			"delete_note":    ack.TierConfirm,
			"drop_all_notes": ack.TierDeny,
		}),
		ack.WithGrants(ack.MemoryGrants()), // 演示用内存许可源；业务可换自有存储
		ack.WithStaticTools(a.Notepad.Tools()...),
	}
	if cfg.RiskPolicy {
		gwOpts = append(gwOpts, ack.WithPolicies(riskPolicy{}))
	}
	a.Gateway = ack.New(svc, gwOpts...)

	aguiRunner := aguirunner.New(a.coreRunner,
		aguirunner.WithAppName(AppName),
		aguirunner.WithSessionService(svc),
		aguirunner.WithRunOptionResolver(a.Gateway.RunOptionResolver()),
		aguirunner.WithMessagesSnapshotFollowEnabled(true),
		aguirunner.WithMessagesSnapshotFollowMaxDuration(30*time.Minute),
		aguirunner.WithFlushInterval(100*time.Millisecond),
		aguirunner.WithTimeout(10*time.Minute), // 未决确认最终由超时结算为拒绝
	)
	if err := a.Gateway.Bind(aguiRunner); err != nil {
		return nil, fmt.Errorf("bind resume runner: %w", err)
	}

	server, err := agui.New(a.coreRunner,
		agui.WithAppName(AppName),
		agui.WithSessionService(svc),
		agui.WithPath("/"),
		agui.WithMessagesSnapshotEnabled(true),
		agui.WithMessagesSnapshotPath("/history"),
		agui.WithCancelEnabled(true),
		agui.WithRunnerFactory(func(runner.Runner, ...aguirunner.Option) (aguirunner.Runner, error) {
			return aguiRunner, nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create agui server: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/agui/", http.StripPrefix("/agui", server.Handler()))
	ackhttp.Mount(mux, a.Gateway) // POST /ack/confirmation、GET /ack/confirmations
	registerSessionAPI(mux, svc)  // GET /api/sessions、DELETE /api/sessions/{id}
	mux.Handle("/", webHandler())
	a.Handler = mux
	return a, nil
}

// Close 释放后台资源：core runner、自有会话存储。
func (a *App) Close() {
	if a.coreRunner != nil {
		_ = a.coreRunner.Close()
	}
	// 通过 SessionServiceOverride 注入的会话存储归调用方所有
	//（测试让两个 App 实例共享同一存储来模拟重启）。
	if a.Sessions != nil && a.Config.SessionServiceOverride == nil {
		_ = a.Sessions.Close()
	}
}

// buildSessionService 按配置选择会话存储：测试可用 Override 直接注入；
// "memory" 走内存实现；其余走 PostgreSQL（目标库不存在时自动创建）。
func (a *App) buildSessionService(ctx context.Context) (session.Service, error) {
	if a.Config.SessionServiceOverride != nil {
		return a.Config.SessionServiceOverride, nil
	}
	if a.Config.SessionBackend == "memory" {
		return inmemory.NewSessionService(), nil
	}
	if err := ensureDatabase(ctx, a.Config.PostgresDSN); err != nil {
		return nil, fmt.Errorf("ensure postgres database: %w", err)
	}
	svc, err := postgres.NewService(postgres.WithPostgresClientDSN(a.Config.PostgresDSN))
	if err != nil {
		return nil, fmt.Errorf("create postgres session service: %w", err)
	}
	return svc, nil
}

// ensureDatabase 先直连目标库试探；连不上则改连维护库 postgres，
// 目标库不存在时创建之，保证全新的 PostgreSQL 环境开箱即用。
func ensureDatabase(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	if err := db.PingContext(ctx); err == nil {
		return db.Close()
	}
	_ = db.Close()

	admin, err := sql.Open("pgx", withDefaultDB(dsn))
	if err != nil {
		return err
	}
	defer admin.Close()

	var name string
	if u, perr := url.Parse(dsn); perr == nil {
		name = strings.TrimPrefix(u.Path, "/")
	}
	if name == "" {
		return fmt.Errorf("cannot derive database name from DSN")
	}
	var exists bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
			return err
		}
		slog.Info("created database", "name", name)
	}
	return nil
}

// withDefaultDB 把 DSN 的库名替换为维护库 postgres，用于建库前连接。
func withDefaultDB(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/postgres"
	return u.String()
}

// quoteIdent 把标识符转为带双引号的 SQL 安全形式（内部双引号翻倍转义）。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
