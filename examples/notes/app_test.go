package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/session"
	"trpc.group/trpc-go/trpc-agent-go/session/inmemory"

	"github.com/jninng/trpc_agent_go_ack"
	"github.com/jninng/trpc_agent_go_ack/acktest"
)

// defaultScript 是"读取 → 删除 → 删除 → 总结"的默认脚本，
// 覆盖 allow 层级、确认与 remember 免确认三条路径。
func defaultScript() []acktest.Step {
	return DefaultMockScript()
}

// deleteScript 构造"删除一条笔记"的两步脚本。
func deleteScript() []acktest.Step {
	return []acktest.Step{
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_del", Name: "delete_note", Args: `{"note_id":"n2"}`}}},
		{Text: "操作已完成。"},
	}
}

// newTestApp 用内存会话存储与脚本化模型组装测试应用，并启动 httptest 服务。
func newTestApp(t *testing.T, svc session.Service, script []acktest.Step) (*App, *httptest.Server) {
	t.Helper()
	return newTestAppMode(t, svc, script, "function")
}

// newTestAppMode 同 newTestApp，但可指定工具接入模式（"function" | "mcp"）。
func newTestAppMode(t *testing.T, svc session.Service, script []acktest.Step, toolMode string) (*App, *httptest.Server) {
	t.Helper()
	if svc == nil {
		svc = inmemory.NewSessionService()
	}
	cfg := Config{
		SessionBackend:         "memory",
		LLM:                    "mock",
		ToolMode:               toolMode,
		MockScript:             script,
		SessionServiceOverride: svc,
	}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })
	return a, ts
}

// sseStream 消费一个 SSE POST 响应，把 data 事件归集为 JSON 对象列表。
type sseStream struct {
	mu     sync.Mutex
	events []map[string]any
	done   chan struct{} // 响应体读尽（流结束）时关闭
	cancel context.CancelFunc
}

// postSSE 发起一个 SSE POST 请求并在后台持续收集事件，返回可轮询的流。
func postSSE(t *testing.T, base, path string, body any) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	raw, err := json.Marshal(body)
	if err != nil {
		cancel()
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("post %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		cancel()
		resp.Body.Close()
		t.Fatalf("post %s: status %d: %s", path, resp.StatusCode, b)
	}

	s := &sseStream{done: make(chan struct{}), cancel: cancel}
	// 注册顺序保证在测试服务清理（LIFO）之前先取消请求，
	// 避免失败场景下仍在推流的 follow 处理器阻塞 ts.Close。
	t.Cleanup(s.close)
	go func() {
		defer close(s.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		var data strings.Builder
		flush := func() {
			if data.Len() == 0 {
				return
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(data.String()), &m); err == nil {
				s.mu.Lock()
				s.events = append(s.events, m)
				s.mu.Unlock()
			}
			data.Reset()
		}
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				flush()
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		flush()
	}()
	return s
}

// snapshot 返回已收集事件的副本（并发安全）。
func (s *sseStream) snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.events...)
}

// waitEvent 轮询等待首个满足条件的事件出现，超时则失败并打印全部事件。
func (s *sseStream) waitEvent(t *testing.T, timeout time.Duration, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, ev := range s.snapshot() {
			if pred(ev) {
				return ev
			}
		}
		select {
		case <-s.done:
			// 流已结束；下方做最后一次扫描
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, ev := range s.snapshot() {
		if pred(ev) {
			return ev
		}
	}
	t.Fatalf("timeout waiting for %s; got events: %s", what, s.raw())
	return nil
}

// raw 把已收集事件序列化为便于失败信息打印的字符串。
func (s *sseStream) raw() string {
	var sb strings.Builder
	for _, ev := range s.snapshot() {
		b, _ := json.Marshal(ev)
		sb.Write(b)
		sb.WriteString("\n")
	}
	return sb.String()
}

func (s *sseStream) close() { s.cancel() }

// eventPred 返回匹配指定 type 的事件谓词。
func eventPred(kind string) func(map[string]any) bool {
	return func(ev map[string]any) bool { return ev["type"] == kind }
}

// pendingConfirmation 等待状态为 pending 的 tool_confirmation 自定义事件。
func pendingConfirmation(t *testing.T, s *sseStream) map[string]any {
	t.Helper()
	return s.waitEvent(t, 10*time.Second, "pending tool_confirmation custom event", func(ev map[string]any) bool {
		if ev["type"] != "CUSTOM" || ev["name"] != ack.EventName {
			return false
		}
		v, _ := ev["value"].(map[string]any)
		return v != nil && v["status"] == ack.StatusPending
	})
}

// countEvents 统计满足条件的事件数。
func countEvents(s *sseStream, pred func(map[string]any) bool) int {
	n := 0
	for _, ev := range s.snapshot() {
		if pred(ev) {
			n++
		}
	}
	return n
}

// confirmationID 从自定义事件中提取 confirmationId。
func confirmationID(ev map[string]any) string {
	v, _ := ev["value"].(map[string]any)
	if v == nil {
		return ""
	}
	id, _ := v["confirmationId"].(string)
	return id
}

// chatInput 构造一次 AG-UI 运行请求体（runId 拼时间戳保证唯一）。
func chatInput(threadID, text string) map[string]any {
	return map[string]any{
		"threadId": threadID,
		"runId":    "run-" + threadID + "-" + fmt.Sprint(time.Now().UnixNano()),
		"messages": []map[string]any{
			{"id": "m1", "role": "user", "content": text},
		},
	}
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

// decideConfirmation 经 HTTP 提交一次人工决定（可携带 remember）。
func decideConfirmation(t *testing.T, base, threadID, confirmationID string, approved bool, remember string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"threadId": threadID, "confirmationId": confirmationID, "approved": approved,
	}
	if remember != "" {
		body["remember"] = remember
	}
	return postJSON(t, base, "/ack/confirmation", body)
}

// runConfirmedChat 驱动一次带确认的完整对话：发送消息、等待确认卡片、
// 提交用户决定并等待 RUN_FINISHED，返回该聊天流。
func runConfirmedChat(t *testing.T, base, threadID string, approve bool) *sseStream {
	t.Helper()
	s := postSSE(t, base, "/agui", chatInput(threadID, "删除 n2"))
	pending := pendingConfirmation(t, s)
	id := confirmationID(pending)
	if id == "" {
		t.Fatalf("missing confirmationId in %v", pending)
	}

	code, body := decideConfirmation(t, base, threadID, id, approve, "")
	if code != http.StatusOK {
		t.Fatalf("confirmation failed: %d %v", code, body)
	}

	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))
	return s
}

// pollHistorySnapshot 以"读取快照即断开"的方式轮询 /agui/history，
// 直到快照内容满足条件或超时。快照直接读存储，不依赖 follow 流的
// 自然终止，对上游时间戳缺陷免疫。
func pollHistorySnapshot(t *testing.T, base, threadID, what string, want func(raw string) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s := postSSE(t, base, "/agui/history", map[string]any{
			"threadId": threadID, "runId": "poll-" + fmt.Sprint(time.Now().UnixNano()), "messages": []any{},
		})
		s.waitEvent(t, 10*time.Second, "MESSAGES_SNAPSHOT", eventPred("MESSAGES_SNAPSHOT"))
		raw := s.raw()
		s.close()
		if want(raw) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting history snapshot for %s", what)
}

// TestApproveFlowExecutesTool 验证同意路径：确认通过后工具真正执行，
// 结果事件回流到聊天流。
func TestApproveFlowExecutesTool(t *testing.T) {
	a, ts := newTestApp(t, nil, deleteScript())

	s := runConfirmedChat(t, ts.URL, "t-approve", true)

	sawResult := false
	for _, ev := range s.snapshot() {
		if ev["type"] == "TOOL_CALL_RESULT" {
			sawResult = true
			if c, _ := ev["content"].(string); !strings.Contains(c, `"deleted":true`) {
				t.Fatalf("tool result content: %s", c)
			}
		}
	}
	if !sawResult {
		t.Fatal("expected TOOL_CALL_RESULT event")
	}
	if got := a.Notepad.Deleted; len(got) != 1 || got[0] != "n2" {
		t.Fatalf("expected n2 deleted, got %v", got)
	}
}

// TestDenyFlowSkipsTool 验证拒绝路径：工具不执行，
// 模型收到 status=denied 的工具结果。
func TestDenyFlowSkipsTool(t *testing.T) {
	a, ts := newTestApp(t, nil, deleteScript())

	s := runConfirmedChat(t, ts.URL, "t-deny", false)

	sawDenied := false
	for _, ev := range s.snapshot() {
		if ev["type"] == "TOOL_CALL_RESULT" {
			if c, _ := ev["content"].(string); strings.Contains(c, `"status":"denied"`) {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Fatalf("expected denied tool result, events:\n%s", s.raw())
	}
	if got := a.Notepad.Deleted; len(got) != 0 {
		t.Fatalf("expected no deletion, got %v", got)
	}
}

// TestFullScriptTiersAndRemember 用默认脚本验证完整许可闭环：
// allow 层级直接执行、confirm 层级出卡片、remember 后同会话免确认。
func TestFullScriptTiersAndRemember(t *testing.T) {
	a, ts := newTestApp(t, nil, defaultScript())

	s := postSSE(t, ts.URL, "/agui", chatInput("t-full", "处理笔记"))
	// 第一张卡片：delete n2（read n1 已按 allow 层级直接执行）。
	pending := pendingConfirmation(t, s)
	id := confirmationID(pending)
	if id != "call_2" {
		t.Fatalf("first confirmation should be delete n2, got %v", pending)
	}
	// 同意并记住（会话级）。
	code, body := decideConfirmation(t, ts.URL, "t-full", id, true, "session")
	if code != http.StatusOK || body["path"] != ack.PathLive {
		t.Fatalf("decide: %d %v", code, body)
	}
	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))

	// 整个流只出现一张待确认卡片（终态事件与其配对；delete n3 经许可直接放行）。
	if n := countEvents(s, func(ev map[string]any) bool {
		if ev["type"] != "CUSTOM" || ev["name"] != ack.EventName {
			return false
		}
		v, _ := ev["value"].(map[string]any)
		return v != nil && v["status"] == ack.StatusPending
	}); n != 1 {
		t.Fatalf("expected exactly one pending confirmation card, got %d", n)
	}
	if n := countEvents(s, eventPred("TOOL_CALL_RESULT")); n != 3 {
		t.Fatalf("expected three tool results, got %d", n)
	}
	if got := a.Notepad.Deleted; len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Fatalf("expected n2,n3 deleted, got %v", got)
	}
}

// TestDenyTierRefused 验证 deny 层级不可降级：工具不进入确认流程，
// 模型收到 denied 结果。
func TestDenyTierRefused(t *testing.T) {
	a, ts := newTestApp(t, nil, []acktest.Step{
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_drop", Name: "drop_all_notes", Args: `{}`}}},
		{Text: "被拒绝了"},
	})

	s := postSSE(t, ts.URL, "/agui", chatInput("t-tier-deny", "全删"))
	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))

	if n := countEvents(s, func(ev map[string]any) bool {
		return ev["type"] == "CUSTOM" && ev["name"] == ack.EventName
	}); n != 0 {
		t.Fatal("deny tier must not produce a confirmation card")
	}
	sawDenied := false
	for _, ev := range s.snapshot() {
		if ev["type"] == "TOOL_CALL_RESULT" {
			if c, _ := ev["content"].(string); strings.Contains(c, `"status":"denied"`) {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Fatalf("expected denied tool result, events:\n%s", s.raw())
	}
	if a.Notepad.Dropped {
		t.Fatal("deny tier tool must never execute")
	}
}

// TestHistoryReplay 验证结束的会话经消息快照回放出
// 已确认的工具调用、确认卡片与结果。
func TestHistoryReplay(t *testing.T) {
	_, ts := newTestApp(t, nil, deleteScript())
	runConfirmedChat(t, ts.URL, "t-history", true)

	s := postSSE(t, ts.URL, "/agui/history", map[string]any{
		"threadId": "t-history", "runId": "h1", "messages": []any{},
	})
	s.waitEvent(t, 10*time.Second, "MESSAGES_SNAPSHOT", eventPred("MESSAGES_SNAPSHOT"))

	raw := s.raw()
	for _, want := range []string{"tool_confirmation", `deleted":true`, "删除 n2"} {
		if !strings.Contains(raw, want) && !strings.Contains(raw, strings.ReplaceAll(want, `"`, `\"`)) {
			t.Fatalf("history snapshot missing %q; got:\n%s", want, raw)
		}
	}
	s.close()
}

// TestRefreshRecovery 验证页面刷新恢复（运行存活）：聊天流断开后，
// /history 快照回放待确认卡片，确认后执行结果落入会话历史。
func TestRefreshRecovery(t *testing.T) {
	a, ts := newTestApp(t, nil, deleteScript())

	chat := postSSE(t, ts.URL, "/agui", chatInput("t-refresh", "删除 n2"))
	pending := pendingConfirmation(t, chat)
	id := confirmationID(pending)
	chat.close() // 模拟页面刷新导致的流断开

	// 快照含待确认卡片。
	snap := postSSE(t, ts.URL, "/agui/history", map[string]any{
		"threadId": "t-refresh", "runId": "h1", "messages": []any{},
	})
	snapEvent := snap.waitEvent(t, 10*time.Second, "MESSAGES_SNAPSHOT", eventPred("MESSAGES_SNAPSHOT"))
	if !strings.Contains(mustJSON(snapEvent), ack.EventName) {
		t.Fatalf("snapshot should contain the pending confirmation card: %s", mustJSON(snapEvent))
	}
	snap.close()

	code, body := decideConfirmation(t, ts.URL, "t-refresh", id, true, "")
	if code != http.StatusOK || body["path"] != ack.PathLive {
		t.Fatalf("confirm expected live, got %d %v", code, body)
	}

	// 快照轮询直到结果出现（对 follow 流时间戳缺陷免疫）。
	pollHistorySnapshot(t, ts.URL, "t-refresh", "tool result via snapshot", func(raw string) bool {
		return strings.Contains(raw, `deleted\":true`) || strings.Contains(raw, `"deleted":true`)
	})
	if got := a.Notepad.Deleted; len(got) != 1 || got[0] != "n2" {
		t.Fatalf("expected n2 deleted, got %v", got)
	}
}

// TestRestartRecovery 验证服务重启恢复：阻塞的运行已丢失，
// 持久化的待确认指针让新实例执行工具并续跑对话。
func TestRestartRecovery(t *testing.T) {
	svc := inmemory.NewSessionService()
	a1, ts1 := newTestApp(t, svc, deleteScript())

	chat := postSSE(t, ts1.URL, "/agui", chatInput("t-restart", "删除 n2"))
	pending := pendingConfirmation(t, chat)
	id := confirmationID(pending)
	chat.close() // 原客户端已断开

	// 模拟"重启"：同一会话存储之上的全新 App 实例。
	_, ts2 := newTestApp(t, svc, deleteScript())

	code, pendings := getJSON(t, ts2.URL+"/ack/confirmations?threadId=t-restart")
	if code != http.StatusOK {
		t.Fatalf("pending query: %d %v", code, pendings)
	}
	if len(pendings) != 1 || pendings[0]["live"] != false {
		t.Fatalf("expected one orphan pending, got %v", pendings)
	}

	decideCode, decideBody := decideConfirmation(t, ts2.URL, "t-restart", id, true, "")
	if decideCode != http.StatusOK || decideBody["path"] != ack.PathRecovery {
		t.Fatalf("confirm expected recovery, got %d %v", decideCode, decideBody)
	}

	// 快照轮询直到续跑结果出现。
	pollHistorySnapshot(t, ts2.URL, "t-restart", "resumed run result", func(raw string) bool {
		hasResult := strings.Contains(raw, `deleted\":true`) || strings.Contains(raw, `"deleted":true`)
		hasText := strings.Contains(raw, "操作已完成")
		return hasResult && hasText
	})

	afterCode, after := getJSON(t, ts2.URL+"/ack/confirmations?threadId=t-restart")
	if afterCode != http.StatusOK || len(after) != 0 {
		t.Fatalf("pending should be cleared after resume, got %d %v", afterCode, after)
	}

	// 释放 app1 中仍阻塞的运行，让其 SSE 处理器在测试服务清理等待前退出
	//（这里选拒绝：工具已经由 app2 执行过）。
	if _, err := a1.Gateway.Decide(context.Background(), ack.Decision{
		SessionID: "t-restart", ConfirmationID: id, Approved: false,
	}); err != nil {
		t.Fatalf("release app1 run: %v", err)
	}
}

// TestRiskPolicyVerdictShapes 验证策略源三形态的端到端演示：
// read_note 读取受保护笔记被强制确认（allow 层级被升压）、
// write_note 被背书直接执行（confirm 层级被降级）、
// delete_note 删除受保护笔记被判定拒绝（不可降级）。
func TestRiskPolicyVerdictShapes(t *testing.T) {
	script := []acktest.Step{
		{ToolCalls: []acktest.ToolCallSpec{{ID: "c_read", Name: "read_note", Args: `{"note_id":"n1"}`}}},
		{ToolCalls: []acktest.ToolCallSpec{{ID: "c_write", Name: "write_note", Args: `{"note_id":"n4","content":"新笔记"}`}}},
		{ToolCalls: []acktest.ToolCallSpec{{ID: "c_del", Name: "delete_note", Args: `{"note_id":"n1"}`}}},
		{Text: "完成"},
	}
	cfg := Config{
		SessionBackend:         "memory",
		LLM:                    "mock",
		RiskPolicy:             true,
		MockScript:             script,
		SessionServiceOverride: inmemory.NewSessionService(),
	}
	a, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(func() { ts.Close(); a.Close() })

	s := postSSE(t, ts.URL, "/agui", chatInput("t-risk", "处理笔记"))
	// 第一张且唯一一张卡片：read n1 被策略强制确认。
	pending := pendingConfirmation(t, s)
	if id := confirmationID(pending); id != "c_read" {
		t.Fatalf("forced confirmation expected for read n1, got %v", pending)
	}
	code, body := decideConfirmation(t, ts.URL, "t-risk", "c_read", true, "")
	if code != http.StatusOK {
		t.Fatalf("decide: %d %v", code, body)
	}
	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))

	if n := countEvents(s, func(ev map[string]any) bool {
		if ev["type"] != "CUSTOM" || ev["name"] != ack.EventName {
			return false
		}
		v, _ := ev["value"].(map[string]any)
		return v != nil && v["status"] == ack.StatusPending
	}); n != 1 {
		t.Fatalf("expected exactly one pending card, got %d", n)
	}
	if n := countEvents(s, eventPred("TOOL_CALL_RESULT")); n != 3 {
		t.Fatalf("expected three tool results, got %d", n)
	}
	// write_note 经背书直接执行。
	if got := a.Notepad.Written; len(got) != 1 || got[0] != "n4" {
		t.Fatalf("write_note should execute via endorsement, got %v", got)
	}
	// delete_note n1 被策略拒绝：不执行，模型收到 denied 结果。
	if got := a.Notepad.Deleted; len(got) != 0 {
		t.Fatalf("policy-denied delete must not execute, got %v", got)
	}
	sawDenied := false
	for _, ev := range s.snapshot() {
		if ev["type"] == "TOOL_CALL_RESULT" {
			if c, _ := ev["content"].(string); strings.Contains(c, `"status":"denied"`) {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Fatalf("expected denied result for policy-denied delete, events:\n%s", s.raw())
	}
}

// TestSessionListAndDelete 验证会话列表与删除接口：
// 标题取自首条用户消息，删除后会话不再出现。
func TestSessionListAndDelete(t *testing.T) {
	_, ts := newTestApp(t, nil, deleteScript())
	runConfirmedChat(t, ts.URL, "t-list", true)

	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var items []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&items)
	resp.Body.Close()
	if len(items) != 1 || items[0]["id"] != "t-list" {
		t.Fatalf("expected one session t-list, got %v", items)
	}
	if title, _ := items[0]["title"].(string); !strings.Contains(title, "删除 n2") {
		t.Fatalf("session title should derive from the first user message, got %q", title)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/sessions/t-list", nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status %d", dresp.StatusCode)
	}

	resp, _ = http.Get(ts.URL + "/api/sessions")
	var after []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&after)
	resp.Body.Close()
	if len(after) != 0 {
		t.Fatalf("expected no sessions after delete, got %v", after)
	}
}

// mustJSON 把任意值序列化为字符串（失败信息打印用）。
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- MCP 工具模式 ----
//
// 以下测试把同一批场景在 MCP 工具模式（ToolMode "mcp"，对应 -mcp 标记）
// 下重放：工具经进程内 MCP server（streamable HTTP）执行，ack 许可网关
// 的层级评估、人工确认、remember 许可与恢复路径代执行对 MCP 工具与
// function 工具行为一致。差异仅在工具执行结果的外层形状：MCP 结果
// 包裹为文本内容数组（如 [{"type":"text","text":"{\"deleted\":true,...}"}]），
// 而拒绝结果由框架在工具边界之前合成，形状与 function 模式相同。

// TestMCPModeFullScriptTiersAndRemember 在 MCP 模式下验证完整许可闭环：
// allow 层级直接执行、confirm 层级出卡片、remember 后同会话免确认。
func TestMCPModeFullScriptTiersAndRemember(t *testing.T) {
	a, ts := newTestAppMode(t, nil, defaultScript(), "mcp")
	if a.MCPURL == "" {
		t.Fatal("mcp mode should expose the tool server url")
	}

	s := postSSE(t, ts.URL, "/agui", chatInput("t-mcp-full", "处理笔记"))
	// 第一张卡片：delete n2（read n1 已按 allow 层级直接执行）。
	pending := pendingConfirmation(t, s)
	if id := confirmationID(pending); id != "call_2" {
		t.Fatalf("first confirmation should be delete n2, got %v", pending)
	}
	// 同意并记住（会话级）。
	code, body := decideConfirmation(t, ts.URL, "t-mcp-full", "call_2", true, "session")
	if code != http.StatusOK || body["path"] != ack.PathLive {
		t.Fatalf("decide: %d %v", code, body)
	}
	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))

	// 整个流只出现一张待确认卡片（delete n3 经许可直接放行）。
	if n := countEvents(s, func(ev map[string]any) bool {
		if ev["type"] != "CUSTOM" || ev["name"] != ack.EventName {
			return false
		}
		v, _ := ev["value"].(map[string]any)
		return v != nil && v["status"] == ack.StatusPending
	}); n != 1 {
		t.Fatalf("expected exactly one pending confirmation card, got %d", n)
	}
	if n := countEvents(s, eventPred("TOOL_CALL_RESULT")); n != 3 {
		t.Fatalf("expected three tool results, got %d", n)
	}
	// 执行痕迹与 function 模式完全一致：工具真正经 MCP 执行。
	if got := a.Notepad.Reads; len(got) != 1 || got[0] != "n1" {
		t.Fatalf("expected n1 read, got %v", got)
	}
	if got := a.Notepad.Deleted; len(got) != 2 || got[0] != "n2" || got[1] != "n3" {
		t.Fatalf("expected n2,n3 deleted, got %v", got)
	}
	// 工具结果为 MCP 文本内容包裹的 JSON。
	sawResult := false
	for _, ev := range s.snapshot() {
		if ev["type"] != "TOOL_CALL_RESULT" {
			continue
		}
		if c, _ := ev["content"].(string); strings.Contains(c, `{\"deleted\":true`) {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("expected MCP-wrapped tool result, events:\n%s", s.raw())
	}
}

// TestMCPModeDenyTierRefused 在 MCP 模式下验证 deny 层级不可降级：
// 拒绝由框架在工具边界之前合成（形状与 function 模式一致），MCP 工具
// 永远不会被调用。
func TestMCPModeDenyTierRefused(t *testing.T) {
	a, ts := newTestAppMode(t, nil, []acktest.Step{
		{ToolCalls: []acktest.ToolCallSpec{{ID: "call_drop", Name: "drop_all_notes", Args: `{}`}}},
		{Text: "被拒绝了"},
	}, "mcp")

	s := postSSE(t, ts.URL, "/agui", chatInput("t-mcp-deny", "全删"))
	s.waitEvent(t, 10*time.Second, "RUN_FINISHED", eventPred("RUN_FINISHED"))

	if n := countEvents(s, func(ev map[string]any) bool {
		return ev["type"] == "CUSTOM" && ev["name"] == ack.EventName
	}); n != 0 {
		t.Fatal("deny tier must not produce a confirmation card")
	}
	sawDenied := false
	for _, ev := range s.snapshot() {
		if ev["type"] == "TOOL_CALL_RESULT" {
			if c, _ := ev["content"].(string); strings.Contains(c, `"status":"denied"`) {
				sawDenied = true
			}
		}
	}
	if !sawDenied {
		t.Fatalf("expected denied tool result, events:\n%s", s.raw())
	}
	if a.Notepad.Dropped {
		t.Fatal("deny tier tool must never execute")
	}
}

// TestMCPModeRestartRecovery 在 MCP 模式下验证服务重启恢复：恢复路径
// 经网关的工具解析器代执行 MCP 工具，并以 RoleTool 消息拉起续跑。
func TestMCPModeRestartRecovery(t *testing.T) {
	svc := inmemory.NewSessionService()
	a1, ts1 := newTestAppMode(t, svc, deleteScript(), "mcp")

	chat := postSSE(t, ts1.URL, "/agui", chatInput("t-mcp-restart", "删除 n2"))
	pending := pendingConfirmation(t, chat)
	id := confirmationID(pending)
	chat.close() // 原客户端已断开

	// 模拟"重启"：同一会话存储之上的全新 App 实例（含全新的 MCP 工具服务）。
	a2, ts2 := newTestAppMode(t, svc, deleteScript(), "mcp")

	code, pendings := getJSON(t, ts2.URL+"/ack/confirmations?threadId=t-mcp-restart")
	if code != http.StatusOK {
		t.Fatalf("pending query: %d %v", code, pendings)
	}
	if len(pendings) != 1 || pendings[0]["live"] != false {
		t.Fatalf("expected one orphan pending, got %v", pendings)
	}

	decideCode, decideBody := decideConfirmation(t, ts2.URL, "t-mcp-restart", id, true, "")
	if decideCode != http.StatusOK || decideBody["path"] != ack.PathRecovery {
		t.Fatalf("confirm expected recovery, got %d %v", decideCode, decideBody)
	}

	// 快照轮询直到续跑完成；代执行结果由 a2 的 MCP 工具产生。
	pollHistorySnapshot(t, ts2.URL, "t-mcp-restart", "resumed run result", func(raw string) bool {
		return strings.Contains(raw, "操作已完成")
	})
	if got := a2.Notepad.Deleted; len(got) != 1 || got[0] != "n2" {
		t.Fatalf("recovery should proxy-execute the mcp tool on the new app, got %v", got)
	}

	afterCode, after := getJSON(t, ts2.URL+"/ack/confirmations?threadId=t-mcp-restart")
	if afterCode != http.StatusOK || len(after) != 0 {
		t.Fatalf("pending should be cleared after resume, got %d %v", afterCode, after)
	}

	// 释放 app1 中仍阻塞的运行，让其 SSE 处理器在测试服务清理等待前退出。
	if _, err := a1.Gateway.Decide(context.Background(), ack.Decision{
		SessionID: "t-mcp-restart", ConfirmationID: id, Approved: false,
	}); err != nil {
		t.Fatalf("release app1 run: %v", err)
	}
}
