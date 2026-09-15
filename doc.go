// Package ack 在 trpc-agent-go 的 AG-UI 协议栈之上提供工具调用许可
// （tool permission）能力。
//
// 工具按层级（Tier）分类：默认放行（allow）、需人工确认（confirm）、
// 禁用（deny）。每次工具调用先经许可评估：外部策略源（PolicySource，
// 如风控）可介入裁决，用户预先授予的许可（Grant，会话级/全局级）可
// 满足确认要求，仅当无任何放行依据时才阻塞等待人工决定。
//
// 人工确认的完整生命周期由网关（Gateway）承载：
//
//   - 直接路径（live）：发出确认的运行存活时，决定唤醒阻塞的运行
//     继续执行，事件仍走原始 SSE/follow 流；
//   - 恢复路径（recovery）：运行已不存在（如服务重启）时，由网关
//     代执行工具或合成拒绝结果，并拉起一次续跑运行，前端经
//     /history follow 流读取。
//
// 前端经 AG-UI 自定义事件 tool_confirmation 渲染确认卡片，经
// ackhttp.Mount 挂载的 HTTP 路由提交决定。最小接入：
//
//	gw := ack.New(svc,
//		ack.WithApp("biz"), // 须与 AG-UI runner 的 AppName 一致
//		ack.WithTiers(map[string]ack.Tier{"delete_note": ack.TierConfirm}),
//		ack.WithGrants(ack.MemoryGrants()),
//		ack.WithStaticTools(readNote, deleteNote),
//	)
//	r := aguirunner.New(coreRunner,
//		aguirunner.WithAppName("biz"),
//		aguirunner.WithSessionService(svc),
//		aguirunner.WithRunOptionResolver(gw.RunOptionResolver()),
//	)
//	_ = gw.Bind(r)
//	ackhttp.Mount(mux, gw)
//
// 完整契约（事件、状态存储、HTTP 形状、不变量）见 docs/module_design.md。
package ack
