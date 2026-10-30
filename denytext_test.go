package ack

import (
	"context"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// TestWithDenyTexts_TierDeny_UsesCustomText 验证定制的 TierDisabled
// 文案在层级 deny 时生效。
func TestWithDenyTexts_TierDeny_UsesCustomText(t *testing.T) {
	gw := New(inmemory.NewSessionService(),
		WithTiers(map[string]Tier{"drop_all": TierDeny}),
		WithDenyTexts(DenyTexts{TierDisabled: func(toolName string) string {
			return "custom deny: " + toolName
		}}),
	)
	d, err := gw.PolicyFor(testSession)(context.Background(),
		&tool.PermissionRequest{ToolCallID: "call_1", ToolName: "drop_all"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if d.Reason != "custom deny: drop_all" {
		t.Fatalf("expected custom tier deny reason, got %q", d.Reason)
	}
}

// TestWithDenyTexts_Unset_TierDeny_UsesDefaultText 验证未定制时层级
// deny 回落默认英文文案。
func TestWithDenyTexts_Unset_TierDeny_UsesDefaultText(t *testing.T) {
	gw := New(inmemory.NewSessionService(),
		WithTiers(map[string]Tier{"drop_all": TierDeny}),
	)
	d, err := gw.PolicyFor(testSession)(context.Background(),
		&tool.PermissionRequest{ToolCallID: "call_1", ToolName: "drop_all"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	want := "Tool drop_all is disabled by the permission tier or policy. Do not call this tool again."
	if d.Reason != want {
		t.Fatalf("expected default tier deny reason, got %q", d.Reason)
	}
}

// TestWithDenyTexts_PersistFailed_UsesCustomText 验证定制的 PersistFailed
// 文案在待确认指针持久化失败（会话不存在）时生效。
func TestWithDenyTexts_PersistFailed_UsesCustomText(t *testing.T) {
	gw := New(inmemory.NewSessionService(),
		WithTiers(map[string]Tier{"delete_note": TierConfirm}),
		WithDenyTexts(DenyTexts{PersistFailed: "custom persist failed"}),
	)
	d, err := gw.PolicyFor(testSession)(context.Background(),
		&tool.PermissionRequest{ToolCallID: "call_1", ToolName: "delete_note"})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if d.Reason != "custom persist failed" {
		t.Fatalf("expected custom persist-failed reason, got %q", d.Reason)
	}
}

// TestWithDenyTexts_RecoveryDeny_UsesCustomHumanDeniedText 验证定制的
// HumanDenied 文案在恢复路径人工拒绝时生效。
func TestWithDenyTexts_RecoveryDeny_UsesCustomHumanDeniedText(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")
	go func() { assistantText(events) }()

	stub := acktest.NewResumeRecorder()
	gw2 := New(ts.svc,
		WithApp(testAppName),
		WithUser(testUser),
		WithDenyTexts(DenyTexts{HumanDenied: func(call ToolCall) string {
			return "custom human denied " + call.ToolName
		}}),
	)
	_ = gw2.Bind(stub)
	if _, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: false,
	}); err != nil {
		t.Fatalf("orphan deny: %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one resume call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Content, `"reason":"custom human denied delete_note"`) {
		t.Fatalf("resume content should carry custom human denied text, got %s", calls[0].Content)
	}
}

// TestWithDenyTexts_RecoveryDegraded_UsesCustomUnavailableText 验证定制
// 的 Unavailable 文案在恢复路径缺工具解析器降级拒绝时生效。
func TestWithDenyTexts_RecoveryDegraded_UsesCustomUnavailableText(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")
	go func() { assistantText(events) }()

	stub := acktest.NewResumeRecorder()
	gw2 := New(ts.svc,
		WithApp(testAppName),
		WithUser(testUser),
		WithDenyTexts(DenyTexts{Unavailable: func(toolName, _ string) string {
			return "custom unavailable " + toolName
		}}),
	)
	_ = gw2.Bind(stub)
	if _, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	}); err != nil {
		t.Fatalf("orphan approve: %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one resume call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Content, `"reason":"custom unavailable delete_note"`) {
		t.Fatalf("resume content should carry custom unavailable text, got %s", calls[0].Content)
	}
}
