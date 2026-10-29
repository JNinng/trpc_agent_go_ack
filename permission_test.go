package ack

import (
	"context"
	"errors"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
)

// verdictPolicy 返回固定判定的策略源。
type verdictPolicy struct{ v Verdict }

func (p verdictPolicy) Consult(context.Context, ToolCall) (Verdict, error) { return p.v, nil }

// errPolicy 总是咨询出错的策略源。
type errPolicy struct{}

func (errPolicy) Consult(context.Context, ToolCall) (Verdict, error) {
	return VerdictAbstain, errors.New("risk control unreachable")
}

// evalCall 是评估测试使用的调用视图。
var evalCall = ToolCall{SessionID: "s1", ToolCallID: "c1", ToolName: "delete_note"}

// newEvalGateway 构造评估测试用的网关。
func newEvalGateway(t *testing.T, tier Tier, policies []PolicySource, grants GrantSource) *Gateway {
	t.Helper()
	g := New(inmemory.NewSessionService(),
		WithTiers(map[string]Tier{"delete_note": tier}),
		WithPolicies(policies...),
	)
	if grants != nil {
		g.opts.grants = grants
	}
	return g
}

// mustDisposition 评估并断言处置与依据。
func mustDisposition(t *testing.T, g *Gateway, wantD disposition, wantBasis string) {
	t.Helper()
	d, basis, _ := g.evaluate(context.Background(), evalCall)
	if d != wantD {
		t.Fatalf("disposition = %v (%s), want %v", d, basis, wantD)
	}
	if string(basis) != wantBasis {
		t.Fatalf("basis = %q, want %q", basis, wantBasis)
	}
}

// TestResolveTierPriority 验证分级解析优先级：TierFunc > 静态表 > 默认层级。
func TestResolveTierPriority(t *testing.T) {
	ctx := context.Background()
	g := New(inmemory.NewSessionService(),
		WithDefaultTier(TierConfirm),
		WithTiers(map[string]Tier{"a": TierDeny}),
		WithTierFunc(func(context.Context, ToolCall) Tier { return TierAllow }),
	)
	if got := g.resolveTier(ctx, ToolCall{ToolName: "a"}); got != TierAllow {
		t.Fatalf("tierFunc should win, got %s", got)
	}

	g2 := New(inmemory.NewSessionService(),
		WithDefaultTier(TierConfirm),
		WithTiers(map[string]Tier{"a": TierDeny}),
	)
	if got := g2.resolveTier(ctx, ToolCall{ToolName: "a"}); got != TierDeny {
		t.Fatalf("static tier should win over default, got %s", got)
	}
	if got := g2.resolveTier(ctx, ToolCall{ToolName: "b"}); got != TierConfirm {
		t.Fatalf("default tier expected, got %s", got)
	}
}

// TestEvaluateMatrix 逐行覆盖评估矩阵：deny 绝对、强制确认、
// allow 层级、背书降级、许可命中、基线确认。
func TestEvaluateMatrix(t *testing.T) {
	t.Run("allow tier no verdicts", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierAllow, nil, nil), dispositionAllow, "tier")
	})
	t.Run("allow tier deny verdict", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierAllow, []PolicySource{verdictPolicy{VerdictDeny}}, nil), dispositionDeny, "verdict")
	})
	t.Run("allow tier confirm verdict forced", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierAllow, []PolicySource{verdictPolicy{VerdictConfirm}}, nil), dispositionConfirm, "forced")
	})
	t.Run("confirm tier no verdicts baseline", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierConfirm, nil, nil), dispositionConfirm, "baseline")
	})
	t.Run("confirm tier abstain baseline", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierConfirm, []PolicySource{verdictPolicy{VerdictAbstain}}, nil), dispositionConfirm, "baseline")
	})
	t.Run("confirm tier grant hit", func(t *testing.T) {
		gs := MemoryGrants()
		_ = gs.Save(context.Background(), Grant{UserID: "user", SessionID: "s1", ToolName: "delete_note"})
		mustDisposition(t, newEvalGateway(t, TierConfirm, nil, gs), dispositionAllow, "grant")
	})
	t.Run("confirm tier endorsement", func(t *testing.T) {
		mustDisposition(t, newEvalGateway(t, TierConfirm, []PolicySource{verdictPolicy{VerdictAllow}}, nil), dispositionAllow, "endorsement")
	})
	t.Run("deny tier absolute", func(t *testing.T) {
		gs := MemoryGrants()
		_ = gs.Save(context.Background(), Grant{UserID: "user", SessionID: "s1", ToolName: "delete_note"})
		mustDisposition(t, newEvalGateway(t, TierDeny,
			[]PolicySource{verdictPolicy{VerdictAllow}}, gs), dispositionDeny, "tier")
	})
}

// TestEvaluateVerdictOrderIndependent 验证评估与策略源顺序无关。
func TestEvaluateVerdictOrderIndependent(t *testing.T) {
	denyThenAllow := newEvalGateway(t, TierConfirm,
		[]PolicySource{verdictPolicy{VerdictDeny}, verdictPolicy{VerdictAllow}}, nil)
	mustDisposition(t, denyThenAllow, dispositionDeny, "verdict")

	allowThenDeny := newEvalGateway(t, TierConfirm,
		[]PolicySource{verdictPolicy{VerdictAllow}, verdictPolicy{VerdictDeny}}, nil)
	mustDisposition(t, allowThenDeny, dispositionDeny, "verdict")

	confirmThenAllow := newEvalGateway(t, TierAllow,
		[]PolicySource{verdictPolicy{VerdictConfirm}, verdictPolicy{VerdictAllow}}, nil)
	mustDisposition(t, confirmThenAllow, dispositionConfirm, "forced")
}

// TestEvaluateNoGrantSourceNeverHits 验证未配置许可源时许可步永不命中。
func TestEvaluateNoGrantSourceNeverHits(t *testing.T) {
	g := New(inmemory.NewSessionService(),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
	)
	mustDisposition(t, g, dispositionConfirm, "baseline")
}

// TestEvaluatePolicyErrorFailSafe 验证策略源咨询出错时按 confirm 判定
// 处理（fail-safe，不静默放行）。
func TestEvaluatePolicyErrorFailSafe(t *testing.T) {
	mustDisposition(t, newEvalGateway(t, TierAllow, []PolicySource{errPolicy{}}, nil),
		dispositionConfirm, "forced")
	mustDisposition(t, newEvalGateway(t, TierConfirm, []PolicySource{errPolicy{}}, nil),
		dispositionConfirm, "forced")
}

// TestGrantLookupSessionBeforeGlobal 验证许可查询次序：会话级先于全局级；
// 会话级许可只对所属会话生效。
func TestGrantLookupSessionBeforeGlobal(t *testing.T) {
	ctx := context.Background()
	gs := MemoryGrants()
	_ = gs.Save(ctx, Grant{UserID: "u", SessionID: "", ToolName: "t", CreatedBy: "global"})
	_ = gs.Save(ctx, Grant{UserID: "u", SessionID: "s1", ToolName: "t", CreatedBy: "session"})

	g, ok := gs.Lookup(ctx, GrantQuery{UserID: "u", SessionID: "s1", ToolName: "t"})
	if !ok || g.CreatedBy != "session" {
		t.Fatalf("session grant should win, got %+v ok=%v", g, ok)
	}
	g, ok = gs.Lookup(ctx, GrantQuery{UserID: "u", SessionID: "s2", ToolName: "t"})
	if !ok || g.CreatedBy != "global" {
		t.Fatalf("global grant should serve other sessions, got %+v ok=%v", g, ok)
	}
	if _, ok := gs.Lookup(ctx, GrantQuery{UserID: "other", SessionID: "s1", ToolName: "t"}); ok {
		t.Fatal("grant must be user-scoped")
	}
}

// failingSaveGrants 许可写入总是失败，查询委托给内嵌的许可源。
type failingSaveGrants struct{ GrantSource }

func (failingSaveGrants) Save(context.Context, Grant) error {
	return errors.New("storage down")
}

// injectWaiter 在网关内登记一个等待者，供 Decide 的写通测试使用。
func injectWaiter(t *testing.T, g *Gateway, sessionID, confirmationID, toolName string) {
	t.Helper()
	g.waiters[waiterKey(sessionID, confirmationID)] = &waiter{
		payload: Payload{
			ConfirmationID: confirmationID,
			ToolCallID:     confirmationID,
			ToolName:       toolName,
			SessionID:      sessionID,
			Status:         StatusPending,
		},
		decided: make(chan bool, 1),
	}
}

// createSession 在内存会话存储中创建指定键的会话。
func createSession(t *testing.T, svc session.Service, key session.Key) {
	t.Helper()
	if _, err := svc.CreateSession(context.Background(), key, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

// TestRememberWriteThrough 验证 Remember 写通：同意 + 作用域生成对应
// 许可；拒绝不生成；写入失败不影响本次决定；未配置许可源时被忽略。
func TestRememberWriteThrough(t *testing.T) {
	ctx := context.Background()
	key := session.Key{AppName: "ack", UserID: "user", SessionID: "s1"}

	t.Run("approve with session scope saves session grant", func(t *testing.T) {
		svc := inmemory.NewSessionService()
		createSession(t, svc, key)
		gs := MemoryGrants()
		g := New(svc, WithGrants(gs))
		injectWaiter(t, g, "s1", "c1", "delete_note")

		res, err := g.Decide(ctx, Decision{SessionID: "s1", ConfirmationID: "c1",
			Approved: true, Remember: ScopeSession, DecidedBy: "tester"})
		if err != nil || res.Path != PathLive {
			t.Fatalf("decide: %v %+v", err, res)
		}
		gr, ok := gs.Lookup(ctx, GrantQuery{UserID: "user", SessionID: "s1", ToolName: "delete_note"})
		if !ok {
			t.Fatal("expected session grant after remember")
		}
		if gr.SessionID != "s1" || gr.CreatedBy != "tester" || gr.CreatedAt == 0 {
			t.Fatalf("grant fields: %+v", gr)
		}
	})

	t.Run("approve with global scope saves global grant", func(t *testing.T) {
		svc := inmemory.NewSessionService()
		createSession(t, svc, key)
		gs := MemoryGrants()
		g := New(svc, WithGrants(gs))
		injectWaiter(t, g, "s1", "c2", "delete_note")

		if _, err := g.Decide(ctx, Decision{SessionID: "s1", ConfirmationID: "c2",
			Approved: true, Remember: ScopeGlobal}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		gr, ok := gs.Lookup(ctx, GrantQuery{UserID: "user", SessionID: "other", ToolName: "delete_note"})
		if !ok || gr.SessionID != "" {
			t.Fatalf("expected global grant, got %+v ok=%v", gr, ok)
		}
	})

	t.Run("deny never saves grant", func(t *testing.T) {
		svc := inmemory.NewSessionService()
		createSession(t, svc, key)
		gs := MemoryGrants()
		g := New(svc, WithGrants(gs))
		injectWaiter(t, g, "s1", "c3", "delete_note")

		if _, err := g.Decide(ctx, Decision{SessionID: "s1", ConfirmationID: "c3",
			Approved: false, Remember: ScopeSession}); err != nil {
			t.Fatalf("decide: %v", err)
		}
		if _, ok := gs.Lookup(ctx, GrantQuery{UserID: "user", SessionID: "s1", ToolName: "delete_note"}); ok {
			t.Fatal("denial must not produce a grant")
		}
	})

	t.Run("save failure does not fail the decision", func(t *testing.T) {
		svc := inmemory.NewSessionService()
		createSession(t, svc, key)
		g := New(svc, WithGrants(failingSaveGrants{MemoryGrants()}))
		injectWaiter(t, g, "s1", "c4", "delete_note")

		res, err := g.Decide(ctx, Decision{SessionID: "s1", ConfirmationID: "c4",
			Approved: true, Remember: ScopeGlobal})
		if err != nil || res.Path != PathLive {
			t.Fatalf("decide should succeed despite grant save failure: %v %+v", err, res)
		}
	})

	t.Run("no grant source ignores remember", func(t *testing.T) {
		svc := inmemory.NewSessionService()
		createSession(t, svc, key)
		g := New(svc)
		injectWaiter(t, g, "s1", "c5", "delete_note")

		res, err := g.Decide(ctx, Decision{SessionID: "s1", ConfirmationID: "c5",
			Approved: true, Remember: ScopeSession})
		if err != nil || res.Path != PathLive {
			t.Fatalf("decide should succeed without grant source: %v %+v", err, res)
		}
	})
}
