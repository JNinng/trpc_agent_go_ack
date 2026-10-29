package ack

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jninng/trpc_agent_go_ack/acktest"

	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// observeRecorder 线程安全地收集授权终态事件与回调时刻的 ctx 状态。
type observeRecorder struct {
	mu     sync.Mutex
	events []PermissionEvent
	ctxErr []error
}

func (r *observeRecorder) record(ctx context.Context, e PermissionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	r.ctxErr = append(r.ctxErr, ctx.Err())
}

func (r *observeRecorder) snapshot() ([]PermissionEvent, []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PermissionEvent(nil), r.events...), append([]error(nil), r.ctxErr...)
}

func (r *observeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// 观察测试固定会话 ID，与 evalCall 的会话一致。
const observeSession = "s1"

// observeKey 是观察测试会话的存储键，与网关缺省身份一致。
var observeKey = session.Key{AppName: "ack", UserID: "user", SessionID: observeSession}

// discardLogger 供观察测试静音网关日志，保持断言输出可定位。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// namedPolicy 构造具名策略源。
func namedPolicy(name string, v Verdict) NamedPolicy {
	return NamedPolicy{Name: name, Source: verdictPolicy{v}}
}

// newObserveGateway 构造观察测试网关：预创建 observeSession 会话，
// 使持久化路径可用；观察者缺省挂 rec.record。
func newObserveGateway(t *testing.T, rec *observeRecorder, opts ...Option) *Gateway {
	t.Helper()
	svc := inmemory.NewSessionService()
	createSession(t, svc, observeKey)
	base := []Option{
		WithPermissionObserver(rec.record),
		WithLogger(discardLogger()),
	}
	return New(svc, append(base, opts...)...)
}

// observeCall 经 PolicyFor 驱动一次调用并返回许可决定。
func observeCall(ctx context.Context, g *Gateway, call ToolCall) (tool.PermissionDecision, error) {
	return g.PolicyFor(call.SessionID)(ctx, &tool.PermissionRequest{
		ToolCallID: call.ToolCallID,
		ToolName:   call.ToolName,
		Arguments:  call.Arguments,
	})
}

// observeOutcome 是一次 PolicyFor 调用的结果。
type observeOutcome struct {
	dec tool.PermissionDecision
	err error
}

// blockInConfirm 驱动一次进入确认流的调用并等待其阻塞登记，
// 返回读取结算结果的通道。
func blockInConfirm(t *testing.T, g *Gateway, ctx context.Context, id string) <-chan observeOutcome {
	t.Helper()
	done := make(chan observeOutcome, 1)
	go func() {
		dec, err := observeCall(ctx, g,
			ToolCall{SessionID: observeSession, ToolCallID: id, ToolName: "delete_note"})
		done <- observeOutcome{dec, err}
	}()
	waitFor(t, 5*time.Second, func() bool { return pendingCountIn(t, g, observeSession) == 1 },
		"pending confirmation")
	return done
}

// mustSettle 等待阻塞调用结算并断言许可动作。
func mustSettle(t *testing.T, done <-chan observeOutcome, want tool.PermissionAction) observeOutcome {
	t.Helper()
	select {
	case o := <-done:
		if o.err != nil || o.dec.Action != want {
			t.Fatalf("expected %s settlement, got %+v %v", want, o.dec, o.err)
		}
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for policy settlement")
		return observeOutcome{}
	}
}

// mustSingleEvent 断言恰好一条终态事件并返回其与回调 ctx 状态。
func mustSingleEvent(t *testing.T, rec *observeRecorder) (PermissionEvent, error) {
	t.Helper()
	events, ctxErrs := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly one event, got %d: %+v", len(events), events)
	}
	return events[0], ctxErrs[0]
}

// TestObserveTierAllowNotifiesAllow 测试分级放行产生 tier 放行终态。
func TestObserveTierAllowNotifiesAllow(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"read_note": TierAllow}))

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_read", ToolName: "read_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("expected allow decision, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisTier || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if e.Call.ToolCallID != "c_read" || e.Call.ToolName != "read_note" || e.Call.SessionID != observeSession {
		t.Fatalf("event call mismatch: %+v", e.Call)
	}
}

// TestObserveTierDenyNotifiesDeny 测试分级拦截产生 tier 拒绝终态。
func TestObserveTierDenyNotifiesDeny(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"drop_all": TierDeny}))

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_drop", ToolName: "drop_all"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionDeny {
		t.Fatalf("expected deny decision, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisTier || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveTierAllowBeatsEndorsement 测试依据优先级：allow 层级
// 先于背书生效，事件 Basis 为 tier 而非 endorsement。
func TestObserveTierAllowBeatsEndorsement(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithNamedPolicies(namedPolicy("risk-a", VerdictAllow)),
		WithTiers(map[string]Tier{"delete_note": TierAllow}),
	)

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_t", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("expected allow decision, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisTier || e.Source != "" {
		t.Fatalf("expected tier basis, got %+v", e)
	}
}

// TestObserveVerdictDenyFirstDenySourceWins 测试判定拒绝的来源归属：
// 多具名源并存时事件 Source 为首个 deny 源。
func TestObserveVerdictDenyFirstDenySourceWins(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithNamedPolicies(
			namedPolicy("risk-a", VerdictAbstain),
			namedPolicy("risk-b", VerdictDeny),
			namedPolicy("risk-c", VerdictAllow),
		),
		WithTiers(map[string]Tier{"delete_note": TierAllow}),
	)

	if _, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_v", ToolName: "delete_note"}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	e, _ := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisVerdict || e.Source != "risk-b" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveAnonymousVerdictDenySourceEmpty 测试匿名策略源拒绝时
// 事件 Source 为空。
func TestObserveAnonymousVerdictDenySourceEmpty(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithPolicies(verdictPolicy{VerdictDeny}),
		WithTiers(map[string]Tier{"delete_note": TierAllow}),
	)

	if _, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_v", ToolName: "delete_note"}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	e, _ := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisVerdict || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveEndorsementFirstAllowSourceWins 测试背书放行的来源归属：
// 多具名源并存时事件 Source 为首个 allow 源。
func TestObserveEndorsementFirstAllowSourceWins(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithNamedPolicies(
			namedPolicy("risk-a", VerdictAbstain),
			namedPolicy("risk-b", VerdictAllow),
		),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
	)

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_e", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("expected endorsement allow, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisEndorsement || e.Source != "risk-b" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveAnonymousEndorsementSourceEmpty 测试匿名策略源背书时
// 事件 Source 为空。
func TestObserveAnonymousEndorsementSourceEmpty(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithPolicies(verdictPolicy{VerdictAllow}),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
	)

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_e", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("expected endorsement allow, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisEndorsement || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveGrantHitNotifiesAllow 测试许可命中产生 grant 放行终态，
// 非策略依据 Source 为空。
func TestObserveGrantHitNotifiesAllow(t *testing.T) {
	rec := &observeRecorder{}
	gs := MemoryGrants()
	if err := gs.Save(context.Background(), Grant{
		UserID: "user", SessionID: observeSession, ToolName: "delete_note",
	}); err != nil {
		t.Fatalf("save grant: %v", err)
	}
	g := newObserveGateway(t, rec,
		WithGrants(gs),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
	)

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_g", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("expected grant allow, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisGrant || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveConfirmPendingNotNotified 测试评估层确认在阻塞等待
// 决定期间不通知（canceled 事件在断言之后产生，取消终态本身由
// TestObserveCanceledNotifiesDeny 覆盖）。
func TestObserveConfirmPendingNotNotified(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := blockInConfirm(t, g, ctx, "c_conf")
	if n := rec.count(); n != 0 {
		t.Fatalf("confirm flow must not notify while pending, got %d events", n)
	}

	cancel()
	mustSettle(t, done, tool.PermissionActionDeny)
}

// TestObserveDecideSettlementNotNotified 测试人工决定结算不通知：
// Decide 结算后事件数保持为零。
func TestObserveDecideSettlementNotNotified(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := blockInConfirm(t, g, ctx, "c_conf")

	if _, err := g.Decide(context.Background(), Decision{
		SessionID: observeSession, ConfirmationID: "c_conf", Approved: true,
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	mustSettle(t, done, tool.PermissionActionAllow)
	if n := rec.count(); n != 0 {
		t.Fatalf("decide settlement must not notify, got %d events", n)
	}
}

// TestObserveForcedConfirmDecideNotNotified 测试强制确认路径的
// 人工决定同样不通知：allow 层级被 confirm 判定强制确认后经 Decide
// 结算，事件数为零。
func TestObserveForcedConfirmDecideNotNotified(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithPolicies(verdictPolicy{VerdictConfirm}),
		WithTiers(map[string]Tier{"delete_note": TierAllow}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := blockInConfirm(t, g, ctx, "c_forced")

	if _, err := g.Decide(context.Background(), Decision{
		SessionID: observeSession, ConfirmationID: "c_forced", Approved: true,
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	mustSettle(t, done, tool.PermissionActionAllow)
	if n := rec.count(); n != 0 {
		t.Fatalf("decide settlement must not notify, got %d events", n)
	}
}

// TestObserveDuplicateIDNotifiesAllow 测试重复确认 ID 的防御性放行
// 产生 duplicate 放行终态，事件携带原始调用视图。
func TestObserveDuplicateIDNotifiesAllow(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	injectWaiter(t, g, observeSession, "c_dup", "delete_note")

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_dup", ToolName: "delete_note",
			Arguments: []byte(`{"note_id":"n3"}`)})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("duplicate id must be allowed through, got %+v", dec)
	}
	e, _ := mustSingleEvent(t, rec)
	if !e.Allowed || e.Basis != BasisDuplicate || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if string(e.Call.Arguments) != `{"note_id":"n3"}` {
		t.Fatalf("event arguments mismatch: %s", e.Call.Arguments)
	}
}

// TestObservePersistFailedNotifiesDeny 测试持久化失败产生
// persist-failed 拒绝终态：会话不存在使 savePending 失败。
func TestObservePersistFailedNotifiesDeny(t *testing.T) {
	rec := &observeRecorder{}
	svc := inmemory.NewSessionService() // 不创建会话，持久化必失败
	g := New(svc,
		WithPermissionObserver(rec.record),
		WithLogger(discardLogger()),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
	)

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_pf", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionDeny {
		t.Fatalf("expected deny on persist failure, got %+v", dec)
	}
	e, ctxErr := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisPersistFailed || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if ctxErr != nil {
		t.Fatalf("observer ctx must not be canceled, got %v", ctxErr)
	}
	g.mu.Lock()
	_, still := g.waiters[waiterKey(observeSession, "c_pf")]
	g.mu.Unlock()
	if still {
		t.Fatal("waiter must be removed from the live table after persist failure")
	}
}

// TestObserveCanceledNotifiesDeny 测试取消结算产生 canceled 拒绝终态，
// 且回调 ctx 保证未取消（WithoutCancel 生效）。
func TestObserveCanceledNotifiesDeny(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	ctx, cancel := context.WithCancel(context.Background())

	done := blockInConfirm(t, g, ctx, "c_cancel")

	cancel()
	mustSettle(t, done, tool.PermissionActionDeny)
	e, ctxErr := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisCanceled || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
	if ctxErr != nil {
		t.Fatalf("observer ctx must survive cancellation, got %v", ctxErr)
	}
	if pendingCountIn(t, g, observeSession) != 0 {
		t.Fatal("pending pointer must be cleared after cancel")
	}
}

// TestObserveTimeoutSettlesCanceled 测试运行超时同样以 canceled 终态
// 结算（与显式取消同机制，D3 的真实触发方式）。
func TestObserveTimeoutSettlesCanceled(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	done := blockInConfirm(t, g, ctx, "c_timeout")

	mustSettle(t, done, tool.PermissionActionDeny)
	e, _ := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisCanceled || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveCancelDecideRaceExactlyOneTerminal 测试取消与决定的并发
// 竞态不变量：真实并发下无论谁先认领结算权，每次迭代恰好一条授权
// 终态——决定生效则零事件且放行，取消生效则决定返回 ErrNotFound 且
// 恰好一条 canceled 事件。两种交错的确定性覆盖见
// TestSettleOperationalAgainstDecide。
func TestObserveCancelDecideRaceExactlyOneTerminal(t *testing.T) {
	const iterations = 24
	for i := 0; i < iterations; i++ {
		rec := &observeRecorder{}
		g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
		ctx, cancel := context.WithCancel(context.Background())

		done := blockInConfirm(t, g, ctx, "c_race")
		decDone := make(chan error, 1)
		decide := func() {
			_, err := g.Decide(context.Background(), Decision{
				SessionID: observeSession, ConfirmationID: "c_race", Approved: true,
			})
			decDone <- err
		}
		if i%2 == 0 {
			cancel() // 取消先发起
			go decide()
		} else {
			go decide()
			time.Sleep(5 * time.Millisecond) // 决定结算（内存操作量级）完成后再取消
			cancel()
		}

		var o observeOutcome
		select {
		case o = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: policy did not settle", i)
		}
		var derr error
		select {
		case derr = <-decDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: decide did not return", i)
		}

		events, _ := rec.snapshot()
		if derr == nil {
			if len(events) != 0 {
				t.Fatalf("iteration %d: decide won but %d events emitted: %+v",
					i, len(events), events)
			}
			if o.err != nil || o.dec.Action != tool.PermissionActionAllow {
				t.Fatalf("iteration %d: decide won but policy denied: %+v %v", i, o.dec, o.err)
			}
		} else {
			if !errors.Is(derr, ErrNotFound) {
				t.Fatalf("iteration %d: decide error: %v", i, derr)
			}
			if len(events) != 1 || events[0].Allowed ||
				events[0].Basis != BasisCanceled || events[0].Source != "" {
				t.Fatalf("iteration %d: expected exactly one canceled event, got %+v",
					i, events)
			}
			if o.err != nil || o.dec.Action != tool.PermissionActionDeny {
				t.Fatalf("iteration %d: cancel won but policy allowed: %+v %v", i, o.dec, o.err)
			}
		}
	}
}

// TestSettleOperationalAgainstDecide 白盒确定性单测：操作性终态与人工
// 决定的三种结算权交错，各自收敛于恰好一条授权终态（分支级覆盖，
// 竞态用例只验并发不变量）。
func TestSettleOperationalAgainstDecide(t *testing.T) {
	call := ToolCall{SessionID: observeSession, ToolCallID: "c_w", ToolName: "delete_note"}

	// newRacer 构造网关并登记一个等待者（不经 PolicyFor，直达结算层）。
	newRacer := func(t *testing.T) (*Gateway, *observeRecorder, *waiter) {
		t.Helper()
		rec := &observeRecorder{}
		g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
		w := &waiter{
			payload: Payload{ConfirmationID: "c_w", ToolCallID: "c_w", ToolName: "delete_note",
				SessionID: observeSession, Status: StatusPending},
			decided: make(chan bool, 1),
		}
		g.mu.Lock()
		g.waiters[waiterKey(observeSession, "c_w")] = w
		g.mu.Unlock()
		return g, rec, w
	}

	t.Run("decision settled first honors decision without event", func(t *testing.T) {
		g, rec, w := newRacer(t)
		// 模拟一次已完成的人工决定：认领结算权、写入决定、释放。
		claimed, _ := g.beginSettle(observeSession, "c_w", nil)
		claimed.decided <- true
		g.endSettle(observeSession, "c_w")

		dec, err := g.settleOperational(context.Background(), "user", call, w, BasisCanceled, cancelDenyReason)
		if err != nil || dec.Action != tool.PermissionActionAllow {
			t.Fatalf("expected decision honored, got %+v %v", dec, err)
		}
		if n := rec.count(); n != 0 {
			t.Fatalf("decided settlement must not notify, got %d events", n)
		}
	})

	t.Run("decision in flight honors decision without event", func(t *testing.T) {
		g, rec, w := newRacer(t)
		g.beginSettle(observeSession, "c_w", nil) // 占住结算权，模拟决定在途

		done := make(chan observeOutcome, 1)
		go func() {
			dec, err := g.settleOperational(context.Background(), "user", call, w, BasisCanceled, cancelDenyReason)
			done <- observeOutcome{dec, err}
		}()
		w.decided <- true // settleLive 的决定写入

		select {
		case o := <-done:
			if o.err != nil || o.dec.Action != tool.PermissionActionAllow {
				t.Fatalf("expected decision honored, got %+v %v", o.dec, o.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for settlement")
		}
		g.endSettle(observeSession, "c_w")
		if n := rec.count(); n != 0 {
			t.Fatalf("decided settlement must not notify, got %d events", n)
		}
	})

	t.Run("busy holder not settling this waiter retries claim and cancels", func(t *testing.T) {
		g, rec, w := newRacer(t)
		// 占住结算权但不摘除本等待者（如同同 ID 前任调用的操作性结算
		// 或恢复路径在途）：身份不匹配的占位等待者。
		g.beginSettle(observeSession, "c_w", &waiter{})

		done := make(chan error, 1)
		go func() {
			_, err := g.settleOperational(context.Background(), "user", call, w, BasisCanceled, cancelDenyReason)
			done <- err
		}()
		time.Sleep(20 * time.Millisecond) // 覆盖至少一次退避重试窗口
		g.endSettle(observeSession, "c_w")

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("settleOperational: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for settlement")
		}
		e, _ := mustSingleEvent(t, rec)
		if e.Allowed || e.Basis != BasisCanceled || e.Source != "" {
			t.Fatalf("unexpected event: %+v", e)
		}
	})
}

// TestObserveNilObserverZeroBehavior 测试观察者缺省 nil 零行为：
// 拒绝与重复 ID 路径均正常返回、不 panic。
func TestObserveNilObserverZeroBehavior(t *testing.T) {
	g := New(inmemory.NewSessionService(),
		WithLogger(discardLogger()),
		WithTiers(map[string]Tier{"drop_all": TierDeny, "delete_note": TierConfirm}))

	dec, err := observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_n1", ToolName: "drop_all"})
	if err != nil || dec.Action != tool.PermissionActionDeny {
		t.Fatalf("tier deny without observer: %+v %v", dec, err)
	}

	injectWaiter(t, g, observeSession, "c_n2", "delete_note")
	dec, err = observeCall(context.Background(), g,
		ToolCall{SessionID: observeSession, ToolCallID: "c_n2", ToolName: "delete_note"})
	if err != nil || dec.Action != tool.PermissionActionAllow {
		t.Fatalf("duplicate path without observer: %+v %v", dec, err)
	}
}

// TestObserveDefensiveAllowNotNotified 测试防御性放行边界：无请求或
// 无调用标识的请求直接放行，且不在观察契约内（零事件）。
func TestObserveDefensiveAllowNotNotified(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec, WithTiers(map[string]Tier{"delete_note": TierConfirm}))
	policy := g.PolicyFor(observeSession)

	dec, err := policy(context.Background(), nil)
	if err != nil || dec.Action != tool.PermissionActionAllow {
		t.Fatalf("nil request must be defensively allowed, got %+v %v", dec, err)
	}
	dec, err = policy(context.Background(), &tool.PermissionRequest{ToolCallID: "", ToolName: "delete_note"})
	if err != nil || dec.Action != tool.PermissionActionAllow {
		t.Fatalf("empty call id must be defensively allowed, got %+v %v", dec, err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("defensive allow must not notify, got %d events", n)
	}
}

// TestObserveConsultErrorCanceledSourceEmpty 测试已知信息损失：策略源
// 咨询出错 fail-safe 强制确认后以取消结算，canceled 事件 Source 为空。
func TestObserveConsultErrorCanceledSourceEmpty(t *testing.T) {
	rec := &observeRecorder{}
	g := newObserveGateway(t, rec,
		WithPolicies(errPolicy{}),
		WithTiers(map[string]Tier{"delete_note": TierAllow}),
	)
	ctx, cancel := context.WithCancel(context.Background())

	done := blockInConfirm(t, g, ctx, "c_err")

	cancel()
	mustSettle(t, done, tool.PermissionActionDeny)
	e, _ := mustSingleEvent(t, rec)
	if e.Allowed || e.Basis != BasisCanceled || e.Source != "" {
		t.Fatalf("unexpected event: %+v", e)
	}
}

// TestObserveRecoveryDecideNotNotified 测试恢复路径的人工决定不通知：
// 孤儿确认经 Decide 结算（历史缺失降级为拒绝续跑），事件数为零。
func TestObserveRecoveryDecideNotNotified(t *testing.T) {
	rec := &observeRecorder{}
	svc := inmemory.NewSessionService()
	createSession(t, svc, observeKey)
	p := Payload{ConfirmationID: "c_orphan", ToolCallID: "c_orphan", ToolName: "ghost",
		SessionID: observeSession, Status: StatusPending, CreatedAt: 1}
	if err := svc.UpdateSessionState(context.Background(), observeKey, session.StateMap{
		StateKey: encodePendingMap(map[string]Payload{"c_orphan": p}),
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	g := New(svc, WithPermissionObserver(rec.record), WithLogger(discardLogger()))
	stub := acktest.NewResumeRecorder()
	if err := g.Bind(stub); err != nil {
		t.Fatalf("bind: %v", err)
	}

	res, err := g.Decide(context.Background(), Decision{
		SessionID: observeSession, ConfirmationID: "c_orphan", Approved: true,
	})
	if err != nil || res.Path != PathRecovery {
		t.Fatalf("orphan decide: %v %+v", err, res)
	}
	if calls := stub.Calls(); len(calls) != 1 || !strings.Contains(calls[0].Content, `"status":"denied"`) {
		t.Fatalf("expected degraded denial resume, got %+v", calls)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("recovery decide must not notify, got %d events", n)
	}
}

// TestObserveNamedPoliciesConsultBeforeAnonymous 测试咨询链合成次序：
// 具名源在前、匿名源在后——首个 deny / 首个 allow 归属具名源。
func TestObserveNamedPoliciesConsultBeforeAnonymous(t *testing.T) {
	t.Run("named deny wins over anonymous deny", func(t *testing.T) {
		rec := &observeRecorder{}
		g := newObserveGateway(t, rec,
			WithNamedPolicies(namedPolicy("risk-named", VerdictDeny)),
			WithPolicies(verdictPolicy{VerdictDeny}),
			WithTiers(map[string]Tier{"delete_note": TierAllow}),
		)
		if _, err := observeCall(context.Background(), g,
			ToolCall{SessionID: observeSession, ToolCallID: "c_o1", ToolName: "delete_note"}); err != nil {
			t.Fatalf("policy: %v", err)
		}
		e, _ := mustSingleEvent(t, rec)
		if e.Source != "risk-named" {
			t.Fatalf("named source should be consulted first, got Source %q", e.Source)
		}
	})

	t.Run("named endorsement wins over anonymous endorsement", func(t *testing.T) {
		rec := &observeRecorder{}
		g := newObserveGateway(t, rec,
			WithNamedPolicies(namedPolicy("risk-named", VerdictAllow)),
			WithPolicies(verdictPolicy{VerdictAllow}),
			WithTiers(map[string]Tier{"delete_note": TierConfirm}),
		)
		if _, err := observeCall(context.Background(), g,
			ToolCall{SessionID: observeSession, ToolCallID: "c_o2", ToolName: "delete_note"}); err != nil {
			t.Fatalf("policy: %v", err)
		}
		e, _ := mustSingleEvent(t, rec)
		if e.Basis != BasisEndorsement || e.Source != "risk-named" {
			t.Fatalf("named endorsement should win, got %+v", e)
		}
	})

	t.Run("empty name treated as anonymous", func(t *testing.T) {
		rec := &observeRecorder{}
		g := newObserveGateway(t, rec,
			WithNamedPolicies(NamedPolicy{Source: verdictPolicy{VerdictDeny}}),
			WithTiers(map[string]Tier{"delete_note": TierAllow}),
		)
		if _, err := observeCall(context.Background(), g,
			ToolCall{SessionID: observeSession, ToolCallID: "c_o3", ToolName: "delete_note"}); err != nil {
			t.Fatalf("policy: %v", err)
		}
		e, _ := mustSingleEvent(t, rec)
		if e.Source != "" {
			t.Fatalf("empty name must yield empty Source, got %q", e.Source)
		}
	})
}
