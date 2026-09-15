package ack

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// 测试固定身份与工具调用 ID。
const (
	testAppName  = "ack-test"
	testUser     = "user"
	testSession  = "s-test"
	deleteCallID = "call_del"
)

// testNotepad 是测试用的内存笔记本，记录执行痕迹供断言。
type testNotepad struct {
	mu      sync.Mutex
	notes   map[string]string
	Reads   []string // 真正读取过的笔记 ID
	Deleted []string // 真正删除过的笔记 ID
	Dropped bool     // drop_all 是否执行过
}

func newTestNotepad() *testNotepad {
	return &testNotepad{notes: map[string]string{
		"n1": "note one",
		"n2": "note two",
		"n3": "note three",
	}}
}

type noteInput struct {
	NoteID string `json:"note_id"`
}

func (n *testNotepad) tools() []tool.Tool {
	read := function.NewFunctionTool(n.read,
		function.WithName("read_note"),
		function.WithDescription("读取一条笔记"),
	)
	del := function.NewFunctionTool(n.delete,
		function.WithName("delete_note"),
		function.WithDescription("永久删除一条笔记"),
	)
	drop := function.NewFunctionTool(n.drop,
		function.WithName("drop_all"),
		function.WithDescription("删除全部笔记（演示 deny 层级）"),
	)
	return []tool.Tool{read, del, drop}
}

func (n *testNotepad) read(_ context.Context, in noteInput) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	content, ok := n.notes[in.NoteID]
	if !ok {
		return map[string]any{"found": false, "note_id": in.NoteID}, nil
	}
	n.Reads = append(n.Reads, in.NoteID)
	return map[string]any{"found": true, "note_id": in.NoteID, "content": content}, nil
}

func (n *testNotepad) delete(_ context.Context, in noteInput) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.notes[in.NoteID]; !ok {
		return map[string]any{"deleted": false, "note_id": in.NoteID, "reason": "笔记不存在"}, nil
	}
	delete(n.notes, in.NoteID)
	n.Deleted = append(n.Deleted, in.NoteID)
	return map[string]any{"deleted": true, "note_id": in.NoteID}, nil
}

func (n *testNotepad) drop(context.Context, struct{}) (map[string]any, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.Dropped = true
	return map[string]any{"dropped": true}, nil
}

// testStack 聚合一次网关测试所需的全部组件。
type testStack struct {
	svc     session.Service
	runner  runner.Runner
	notepad *testNotepad
	gw      *Gateway
}

// newTestStack 组装"内存会话 + 脚本化模型 + 许可网关"的测试栈：
// read_note 为 allow 层级，delete_note 为 confirm 层级，drop_all 为
// deny 层级。
func newTestStack(t *testing.T, steps ...acktest.Step) *testStack {
	t.Helper()
	np := newTestNotepad()
	svc := inmemory.NewSessionService()
	ag := llmagent.New("test-agent",
		llmagent.WithModel(acktest.NewScriptedModel("test-model", steps...)),
		llmagent.WithTools(np.tools()),
	)
	return &testStack{
		svc:     svc,
		runner:  runner.NewRunner(testAppName, ag, runner.WithSessionService(svc)),
		notepad: np,
		gw: New(svc,
			WithApp(testAppName),
			WithUser(testUser),
			WithTiers(map[string]Tier{
				"read_note":   TierAllow,
				"delete_note": TierConfirm,
				"drop_all":    TierDeny,
			}),
			WithGrants(MemoryGrants()),
			WithStaticTools(np.tools()...),
		),
	}
}

// runChat 启动一次挂有许可策略的运行并收集事件；
// 返回的通道在运行结束时关闭。
func (ts *testStack) runChat(t *testing.T, ctx context.Context, sessionID, text string) <-chan *event.Event {
	t.Helper()
	events, err := ts.runner.Run(ctx, testUser, sessionID, model.NewUserMessage(text),
		agent.WithToolPermissionPolicyFunc(ts.gw.PolicyFor(sessionID)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := make(chan *event.Event, 128)
	go func() {
		defer close(out)
		for ev := range events {
			out <- ev
		}
	}()
	return out
}

// pendingCountIn 查询指定会话的待确认数量。
func pendingCountIn(t *testing.T, gw *Gateway, sessionID string) int {
	t.Helper()
	items, err := gw.Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	return len(items)
}

func pendingCount(t *testing.T, gw *Gateway) int {
	return pendingCountIn(t, gw, testSession)
}

// waitFor 轮询等待条件成立，超时则失败。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// assistantText 返回事件流中出现的全部助手文本的拼接。
func assistantText(events <-chan *event.Event) string {
	var sb strings.Builder
	for ev := range events {
		if ev == nil || ev.Response == nil {
			continue
		}
		for _, c := range ev.Response.Choices {
			if c.Message.Role == model.RoleAssistant && c.Message.Content != "" {
				sb.WriteString(c.Message.Content)
			}
		}
	}
	return sb.String()
}

// TestGatewayApproveWakesBlockedRun 验证直接路径：同意决定唤醒阻塞的
// 运行，工具执行、运行收尾、待确认清理。
func TestGatewayApproveWakesBlockedRun(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "已删除"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")

	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")

	res, err := ts.gw.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.Path != PathLive {
		t.Fatalf("expected live path, got %q", res.Path)
	}

	done := make(chan string, 1)
	go func() { done <- assistantText(events) }()
	select {
	case text := <-done:
		if !strings.Contains(text, "已删除") {
			t.Fatalf("expected final assistant text, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for run completion")
	}

	if got := ts.notepad.Deleted; len(got) != 1 || got[0] != "n2" {
		t.Fatalf("expected n2 deleted after approval, got %v", got)
	}
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 0 }, "pending cleared")
}

// TestGatewayDenySkipsExecution 验证拒绝：工具不执行，
// 阻塞运行以拒绝结果收尾。
func TestGatewayDenySkipsExecution(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "好的，不删了"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")

	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")

	if _, err := ts.gw.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: false,
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	go func() { assistantText(events) }()
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 0 }, "pending cleared")

	if got := ts.notepad.Deleted; len(got) != 0 {
		t.Fatalf("expected no deletion after denial, got %v", got)
	}
}

// TestGatewayContextCancelCleansUp 验证运行取消：上下文结束时，
// 内存等待者与持久化指针都被清理。
func TestGatewayContextCancelCleansUp(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n1"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	events := ts.runChat(t, ctx, testSession, "删除 n1")

	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")

	cancel()
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 0 }, "pending cleared after cancel")
	go func() { assistantText(events) }()
}

// TestGatewayCrossSessionSameCallID 是回归测试：确认 ID（工具调用 ID）
// 可能跨会话重复；两次调用都必须各自阻塞等待确认，
// 而不是第二个被当作"重复"直接放行。
func TestGatewayCrossSessionSameCallID(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evA := ts.runChat(t, ctx, "sess-a", "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCountIn(t, ts.gw, "sess-a") == 1 }, "pending in sess-a")

	evB := ts.runChat(t, ctx, "sess-b", "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCountIn(t, ts.gw, "sess-b") == 1 }, "pending in sess-b")

	if got := ts.notepad.Deleted; len(got) != 0 {
		t.Fatalf("no tool should have executed without confirmation, got %v", got)
	}

	// 决定 A 不影响 B；两个会话各自的等待者独立。
	if _, err := ts.gw.Decide(context.Background(), Decision{
		SessionID: "sess-a", ConfirmationID: deleteCallID, Approved: false,
	}); err != nil {
		t.Fatalf("decide a: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return pendingCountIn(t, ts.gw, "sess-a") == 0 }, "sess-a cleared")
	if pendingCountIn(t, ts.gw, "sess-b") != 1 {
		t.Fatal("sess-b pending must survive sess-a decision")
	}
	go func() { assistantText(evA) }()
	go func() { assistantText(evB) }()
}

// TestGatewayConcurrentDecideFirstWins 验证同一确认的并发决定竞争：
// 先到者生效，后到者返回 ErrNotFound，工具只执行一次。
func TestGatewayConcurrentDecideFirstWins(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")

	const workers = 8
	errs := make(chan error, workers)
	var successes atomic.Int32
	for i := 0; i < workers; i++ {
		go func() {
			_, err := ts.gw.Decide(context.Background(), Decision{
				SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
			})
			if err == nil {
				successes.Add(1)
			}
			errs <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("concurrent decide error: %v", err)
		}
	}
	if n := successes.Load(); n != 1 {
		t.Fatalf("exactly one decide should win, got %d", n)
	}
	go func() { assistantText(events) }()
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 0 }, "pending cleared")
	// 工具在运行 goroutine 内执行：等待运行结束后在锁内断言。
	waitFor(t, 5*time.Second, func() bool {
		ts.notepad.mu.Lock()
		defer ts.notepad.mu.Unlock()
		return len(ts.notepad.Deleted) == 1
	}, "tool executed exactly once")
}

// TestGatewayAllowTierExecutesDirectly 验证 allow 层级直接放行：
// 不产生确认，工具立即执行。
func TestGatewayAllowTierExecutesDirectly(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: "call_read", Name: "read_note", Args: `{"note_id":"n1"}`}}},
		acktest.Step{Text: "已读取"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "读取 n1")

	done := make(chan string, 1)
	go func() { done <- assistantText(events) }()
	select {
	case text := <-done:
		if !strings.Contains(text, "已读取") {
			t.Fatalf("expected final assistant text, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for run completion")
	}
	if pendingCount(t, ts.gw) != 0 {
		t.Fatal("allow tier must not produce a confirmation")
	}
	if got := ts.notepad.Reads; len(got) != 1 || got[0] != "n1" {
		t.Fatalf("expected n1 read without confirmation, got %v", got)
	}
}

// TestGatewayDenyTierSkipsConfirmation 验证 deny 层级不可降级：
// 不进入确认流程，工具不执行，模型收到拒绝结果。
func TestGatewayDenyTierSkipsConfirmation(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: "call_drop", Name: "drop_all", Args: `{}`}}},
		acktest.Step{Text: "被拒绝了"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "全删")

	done := make(chan string, 1)
	go func() { done <- assistantText(events) }()
	select {
	case text := <-done:
		if !strings.Contains(text, "被拒绝了") {
			t.Fatalf("expected final assistant text, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for run completion")
	}
	if ts.notepad.Dropped {
		t.Fatal("deny tier tool must never execute")
	}
	if pendingCount(t, ts.gw) != 0 {
		t.Fatal("deny tier must not produce a confirmation")
	}
}

// TestGatewayRememberSkipsNextConfirmation 验证许可闭环：第一次确认同意
// 时勾选 remember（会话级），同会话下一次同工具调用免确认直接执行。
func TestGatewayRememberSkipsNextConfirmation(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: "call_d1", Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: "call_d2", Name: "delete_note", Args: `{"note_id":"n3"}`}}},
		acktest.Step{Text: "都删完了"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2 和 n3")

	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "first pending confirmation")
	if _, err := ts.gw.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: "call_d1", Approved: true, Remember: ScopeSession,
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	// 第二次删除命中会话级许可：不再出现待确认，运行自然完成。
	done := make(chan string, 1)
	go func() { done <- assistantText(events) }()
	select {
	case text := <-done:
		if !strings.Contains(text, "都删完了") {
			t.Fatalf("expected final assistant text, got %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for run completion (second delete should not block)")
	}
	if got := ts.notepad.Deleted; len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Fatalf("expected n2,n3 deleted via grant, got %v", got)
	}
}

// TestGatewayOrphanDecideExecutesAndResumes 验证恢复路径：原运行不在时，
// 新网关依据持久化待确认指针执行工具并拉起续跑运行。
func TestGatewayOrphanDecideExecutesAndResumes(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")
	go func() { assistantText(events) }()

	// 模拟重启：同一会话存储之上的全新网关。
	stub := acktest.NewResumeRecorder()
	gw2 := New(ts.svc,
		WithApp(testAppName),
		WithUser(testUser),
		WithStaticTools(ts.notepad.tools()...),
	)
	if err := gw2.Bind(stub); err != nil {
		t.Fatalf("bind: %v", err)
	}

	res, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	})
	if err != nil {
		t.Fatalf("orphan decide: %v", err)
	}
	if res.Path != PathRecovery {
		t.Fatalf("expected recovery path, got %q", res.Path)
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one resume call, got %d", len(calls))
	}
	if calls[0].ThreadID != testSession || calls[0].ToolCallID != deleteCallID {
		t.Fatalf("resume call mismatch: %+v", calls[0])
	}
	if !strings.Contains(calls[0].Content, `"deleted":true`) {
		t.Fatalf("resume content should carry the executed result, got %s", calls[0].Content)
	}
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, gw2) == 0 }, "pending cleared")
	if got := ts.notepad.Deleted; len(got) != 1 || got[0] != "n2" {
		t.Fatalf("orphan approve should execute the tool, got %v", got)
	}
}

// TestGatewayOrphanDenySynthesizesResult 验证孤儿拒绝：工具不执行，
// 续跑消息携带 denied 结果。
func TestGatewayOrphanDenySynthesizesResult(t *testing.T) {
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
		WithStaticTools(ts.notepad.tools()...),
	)
	_ = gw2.Bind(stub)

	res, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: false,
	})
	if err != nil || res.Path != PathRecovery {
		t.Fatalf("orphan deny: %v %+v", err, res)
	}
	calls := stub.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected one resume call, got %d", len(calls))
	}
	if !strings.Contains(calls[0].Content, `"status":"denied"`) {
		t.Fatalf("resume content should be a denied result, got %s", calls[0].Content)
	}
	if got := ts.notepad.Deleted; len(got) != 0 {
		t.Fatalf("orphan deny must not execute the tool, got %v", got)
	}
}

// TestGatewayOrphanNotBound 验证未 Bind 的网关在恢复路径触发时返回
// ErrNotBound，且不清除待确认指针。
func TestGatewayOrphanNotBound(t *testing.T) {
	ts := newTestStack(t,
		acktest.Step{ToolCalls: []acktest.ToolCallSpec{{ID: deleteCallID, Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		acktest.Step{Text: "done"},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := ts.runChat(t, ctx, testSession, "删除 n2")
	waitFor(t, 5*time.Second, func() bool { return pendingCount(t, ts.gw) == 1 }, "pending confirmation")
	go func() { assistantText(events) }()

	gw2 := New(ts.svc, WithApp(testAppName), WithUser(testUser))
	_, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	})
	if !errors.Is(err, ErrNotBound) {
		t.Fatalf("expected ErrNotBound, got %v", err)
	}
	// 指针保留：补 Bind 后同一决定可以重试。
	stub := acktest.NewResumeRecorder()
	_ = gw2.Bind(stub)
	res, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	})
	if err != nil || res.Path != PathRecovery {
		t.Fatalf("retry after bind: %v %+v", err, res)
	}
}

// TestGatewayOrphanRunActive 验证续跑冲突错误包装为 ErrRunActive，
// errors.Is 可判。
func TestGatewayOrphanRunActive(t *testing.T) {
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
	stub.FailWith(aguirunner.ErrRunAlreadyExists)
	gw2 := New(ts.svc, WithApp(testAppName), WithUser(testUser))
	_ = gw2.Bind(stub)

	_, err := gw2.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: deleteCallID, Approved: true,
	})
	if !errors.Is(err, ErrRunActive) || !errors.Is(err, aguirunner.ErrRunAlreadyExists) {
		t.Fatalf("expected ErrRunActive wrapping the framework conflict, got %v", err)
	}
}

// TestGatewayOrphanHistoryMissingDegradesToDenial 验证历史中找不回原始
// 调用时降级为拒绝续跑（安全优先，不执行未知调用）。
func TestGatewayOrphanHistoryMissingDegradesToDenial(t *testing.T) {
	svc := inmemory.NewSessionService()
	key := session.Key{AppName: testAppName, UserID: testUser, SessionID: testSession}
	if _, err := svc.CreateSession(context.Background(), key, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// 只写待确认指针，不写工具调用历史。
	p := Payload{ConfirmationID: "c_ghost", ToolCallID: "c_ghost", ToolName: "ghost",
		SessionID: testSession, Status: StatusPending, CreatedAt: 1}
	if err := svc.UpdateSessionState(context.Background(), key, session.StateMap{
		StateKey: encodePendingMap(map[string]Payload{"c_ghost": p}),
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	stub := acktest.NewResumeRecorder()
	gw := New(svc, WithApp(testAppName), WithUser(testUser))
	_ = gw.Bind(stub)

	res, err := gw.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: "c_ghost", Approved: true,
	})
	if err != nil || res.Path != PathRecovery {
		t.Fatalf("ghost orphan decide: %v %+v", err, res)
	}
	calls := stub.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Content, `"status":"denied"`) {
		t.Fatalf("missing history must degrade to denial, got %+v", calls)
	}
}

// TestGatewayUnknownConfirmation 验证不存在的确认返回 ErrNotFound。
func TestGatewayUnknownConfirmation(t *testing.T) {
	ts := newTestStack(t)
	if _, err := ts.gw.Decide(context.Background(), Decision{
		SessionID: testSession, ConfirmationID: "nope",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestGatewayBindNil 验证 Bind(nil) 返回错误。
func TestGatewayBindNil(t *testing.T) {
	ts := newTestStack(t)
	if err := ts.gw.Bind(nil); err == nil {
		t.Fatal("Bind(nil) must return an error")
	}
}

// TestGatewayDuplicateConfirmationIDAllows 验证同会话重复确认 ID：
// 首个确认仍在等待时，同 ID 的后到调用直接放行，不进入确认流程。
func TestGatewayDuplicateConfirmationIDAllows(t *testing.T) {
	ts := newTestStack(t)
	injectWaiter(t, ts.gw, testSession, deleteCallID, "delete_note")

	dec, err := ts.gw.PolicyFor(testSession)(context.Background(), &tool.PermissionRequest{
		ToolCallID: deleteCallID,
		ToolName:   "delete_note",
		Arguments:  []byte(`{"note_id":"n3"}`),
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if dec.Action != tool.PermissionActionAllow {
		t.Fatalf("duplicate confirmation id must be allowed through, got %+v", dec)
	}
	// 不产生第二个等待者或待确认项。
	if pendingCount(t, ts.gw) != 0 {
		t.Fatal("duplicate id must not create a new pending confirmation")
	}
}
