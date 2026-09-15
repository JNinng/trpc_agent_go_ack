package ackhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	aguirunner "trpc.group/trpc-go/trpc-agent-go/server/agui/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/jninng/trpc_agent_go_ack"
	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// testEnv 聚合 HTTP 集成测试所需组件。
type testEnv struct {
	svc    session.Service
	grants ack.GrantSource
	gw     *ack.Gateway
	stub   *acktest.ResumeRecorder
	ts     *httptest.Server
}

// newTestEnv 组装"内存会话 + 内存许可 + 记录式续跑"的测试环境，
// 路由挂载在默认前缀 /ack 下。
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	svc := inmemory.NewSessionService()
	grants := ack.MemoryGrants()
	gw := ack.New(svc,
		ack.WithApp("ack-http-test"),
		ack.WithUser("u1"),
		ack.WithTiers(map[string]ack.Tier{"delete_note": ack.TierConfirm}),
		ack.WithGrants(grants),
	)
	stub := acktest.NewResumeRecorder()
	if err := gw.Bind(stub); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mux := http.NewServeMux()
	Mount(mux, gw)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &testEnv{svc: svc, grants: grants, gw: gw, stub: stub, ts: ts}
}

// createSession 在会话存储中创建指定 ID 的会话。
func (e *testEnv) createSession(t *testing.T, sessionID string) {
	t.Helper()
	if _, err := e.svc.CreateSession(context.Background(),
		session.Key{AppName: "ack-http-test", UserID: "u1", SessionID: sessionID}, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

// blockInPolicy 经公开 API 造一个存活的等待者：在独立 goroutine 里
// 调用权限策略，它会登记待确认项并阻塞直到被决定或 ctx 结束。
func (e *testEnv) blockInPolicy(t *testing.T, sessionID, confirmationID, toolName string) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	policy := e.gw.PolicyFor(sessionID)
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		_, _ = policy(ctx, &tool.PermissionRequest{
			ToolCallID: confirmationID,
			ToolName:   toolName,
			Arguments:  []byte(`{}`),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-blocked:
		case <-time.After(5 * time.Second):
		}
	})
	return cancel, blocked
}

// postJSON 发送 JSON POST，返回状态码与解码后的响应体。
func postJSON(t *testing.T, base, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// getJSON 发送 GET，返回状态码与 JSON 数组响应。
func getJSON(t *testing.T, url string) (int, []map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestDecideValidation 验证决定路由的请求校验：形状缺失与非法 remember。
func TestDecideValidation(t *testing.T) {
	e := newTestEnv(t)

	code, body := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"confirmationId": "c1", "approved": true,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing threadId: %d %v", code, body)
	}

	code, body = postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t1", "approved": true,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("missing confirmationId: %d %v", code, body)
	}

	code, body = postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t1", "confirmationId": "c1", "approved": true, "remember": "forever",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid remember: %d %v", code, body)
	}
}

// TestDecideUnknownConfirmation 验证不存在的确认返回 404。
func TestDecideUnknownConfirmation(t *testing.T) {
	e := newTestEnv(t)
	e.createSession(t, "t1")

	code, body := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t1", "confirmationId": "nope", "approved": true,
	})
	if code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %v", code, body)
	}
}

// TestDecideLivePath 验证直接路径：决定唤醒阻塞的策略调用，
// 返回 {"path":"live"}；同意 + remember=session 写入会话级许可。
func TestDecideLivePath(t *testing.T) {
	e := newTestEnv(t)
	e.createSession(t, "t-live")
	_, blocked := e.blockInPolicy(t, "t-live", "c1", "delete_note")

	// 等待待确认项出现且 live=true。
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, items := getJSON(t, e.ts.URL+"/ack/confirmations?threadId=t-live")
		if code == http.StatusOK && len(items) == 1 && items[0]["live"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for pending item, got %d %v", code, items)
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, body := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t-live", "confirmationId": "c1", "approved": true, "remember": "session",
	})
	if code != http.StatusOK || body["path"] != ack.PathLive {
		t.Fatalf("decide: %d %v", code, body)
	}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked policy call should wake after decide")
	}
	gr, ok := e.grants.Lookup(context.Background(), ack.GrantQuery{
		UserID: "u1", SessionID: "t-live", ToolName: "delete_note"})
	if !ok || gr.SessionID != "t-live" {
		t.Fatalf("expected session grant, got %+v ok=%v", gr, ok)
	}
}

// seedOrphan 在会话状态中直接写入一条孤儿待确认指针。
func (e *testEnv) seedOrphan(t *testing.T, sessionID, confirmationID, toolName string) {
	t.Helper()
	e.createSession(t, sessionID)
	p := ack.Payload{
		ConfirmationID: confirmationID,
		ToolCallID:     confirmationID,
		ToolName:       toolName,
		SessionID:      sessionID,
		Status:         ack.StatusPending,
		CreatedAt:      time.Now().UnixMilli(),
	}
	raw, _ := json.Marshal(map[string]ack.Payload{confirmationID: p})
	if err := e.svc.UpdateSessionState(context.Background(),
		session.Key{AppName: "ack-http-test", UserID: "u1", SessionID: sessionID},
		session.StateMap{ack.StateKey: raw}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
}

// TestDecideRecoveryPath 验证恢复路径：孤儿确认被决定后返回
// {"path":"recovery"}，续跑被拉起；拒绝时合成 denied 结果。
func TestDecideRecoveryPath(t *testing.T) {
	e := newTestEnv(t)
	e.seedOrphan(t, "t-orphan", "c9", "delete_note")

	code, items := getJSON(t, e.ts.URL+"/ack/confirmations?threadId=t-orphan")
	if code != http.StatusOK || len(items) != 1 || items[0]["live"] != false {
		t.Fatalf("orphan pending expected live=false, got %d %v", code, items)
	}

	code, body := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t-orphan", "confirmationId": "c9", "approved": false,
	})
	if code != http.StatusOK || body["path"] != ack.PathRecovery {
		t.Fatalf("decide: %d %v", code, body)
	}
	calls := e.stub.Calls()
	if len(calls) != 1 || calls[0].ThreadID != "t-orphan" {
		t.Fatalf("resume call: %+v", calls)
	}
	if !strings.Contains(calls[0].Content, `"status":"denied"`) {
		t.Fatalf("denied content expected, got %s", calls[0].Content)
	}
}

// TestDecideRunActive 验证续跑冲突错误映射为 409。
func TestDecideRunActive(t *testing.T) {
	e := newTestEnv(t)
	e.seedOrphan(t, "t-409", "c8", "delete_note")
	e.stub.FailWith(aguirunner.ErrRunAlreadyExists)

	code, body := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t-409", "confirmationId": "c8", "approved": true,
	})
	if code != http.StatusConflict {
		t.Fatalf("expected 409, got %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "run active") {
		t.Fatalf("error should carry ErrRunActive, got %v", body)
	}
}

// TestListPendingValidation 验证待确认查询的请求校验与响应形状。
func TestListPendingValidation(t *testing.T) {
	e := newTestEnv(t)

	code, _ := getJSON(t, e.ts.URL+"/ack/confirmations")
	if code != http.StatusBadRequest {
		t.Fatalf("missing threadId expected 400, got %d", code)
	}

	e.seedOrphan(t, "t-shape", "c7", "read_note")
	code, items := getJSON(t, e.ts.URL+"/ack/confirmations?threadId=t-shape")
	if code != http.StatusOK || len(items) != 1 {
		t.Fatalf("pending list: %d %v", code, items)
	}
	item := items[0]
	for _, key := range []string{"confirmationId", "toolCallId", "toolName", "sessionId", "status", "createdAt", "live"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("pending item missing %q: %v", key, item)
		}
	}
	if item["status"] != ack.StatusPending || item["live"] != false {
		t.Fatalf("pending item shape: %v", item)
	}
}

// TestCustomPrefix 验证 WithPrefix 自定义前缀。
func TestCustomPrefix(t *testing.T) {
	svc := inmemory.NewSessionService()
	gw := ack.New(svc, ack.WithApp("pfx"), ack.WithUser("u1"))
	mux := http.NewServeMux()
	Mount(mux, gw, WithPrefix("/api"))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/confirmations")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("custom prefix route expected 400 (missing threadId), got %d", resp.StatusCode)
	}
}

// TestRememberInvalidIs400 属校验回归：非法 remember 值不进网关。
func TestRememberInvalidIs400(t *testing.T) {
	e := newTestEnv(t)
	e.createSession(t, "t2")
	code, _ := postJSON(t, e.ts.URL, "/ack/confirmation", map[string]any{
		"threadId": "t2", "confirmationId": "c1", "approved": false, "remember": "SESSION",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("uppercase remember must be rejected: %d", code)
	}
}
