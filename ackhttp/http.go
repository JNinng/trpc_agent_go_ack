// Package ackhttp 把确认决定/查询路由挂载到任意 http.ServeMux，
// 仅依赖标准库与 ack。身份从请求上下文解析：鉴权中间件将用户写入
// ctx，网关经 WithUserFunc 取值；未配置动态身份时使用静态 WithUser 值。
package ackhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jninng/trpc_agent_go_ack"
)

// Mount 把确认路由挂载到 mux：
//
//	POST {p}/confirmation     提交人工决定（同意/拒绝，可选 remember）
//	GET  {p}/confirmations    查询会话的待确认项
//
// 前缀默认 "/ack"，可用 WithPrefix 调整。
func Mount(mux *http.ServeMux, g *ack.Gateway, opts ...Option) {
	o := &options{prefix: "/ack"}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	p := strings.TrimSuffix(o.prefix, "/")
	h := &handlers{gw: g}
	mux.HandleFunc("POST "+p+"/confirmation", h.decide)
	mux.HandleFunc("GET "+p+"/confirmations", h.listPending)
}

// Option 配置路由挂载。
type Option func(*options)

type options struct {
	prefix string
}

// WithPrefix 设置路由前缀（默认 "/ack"）。
func WithPrefix(p string) Option {
	return func(o *options) { o.prefix = p }
}

// handlers 持有网关并实现各路由的处理函数。
type handlers struct {
	gw *ack.Gateway
}

// decideRequest 是 POST {p}/confirmation 的请求体。
type decideRequest struct {
	ThreadID       string `json:"threadId"`           // 会话 ID
	ConfirmationID string `json:"confirmationId"`     // 确认 ID
	Approved       bool   `json:"approved"`           // true=同意，false=拒绝
	Remember       string `json:"remember,omitempty"` // 可选："session" / "global"
}

// decide 处理决定路由：把请求转交给网关结算，返回实际生效的路径。
// 哨兵错误映射状态码：ErrNotFound → 404，ErrRunActive → 409。
func (h *handlers) decide(w http.ResponseWriter, r *http.Request) {
	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ThreadID == "" || req.ConfirmationID == "" {
		writeError(w, http.StatusBadRequest, errors.New("threadId and confirmationId are required"))
		return
	}
	var remember ack.Scope
	switch req.Remember {
	case "":
	case string(ack.ScopeSession):
		remember = ack.ScopeSession
	case string(ack.ScopeGlobal):
		remember = ack.ScopeGlobal
	default:
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("remember must be %q or %q", ack.ScopeSession, ack.ScopeGlobal))
		return
	}

	res, err := h.gw.Decide(r.Context(), ack.Decision{
		SessionID:      req.ThreadID,
		ConfirmationID: req.ConfirmationID,
		Approved:       req.Approved,
		Remember:       remember,
	})
	switch {
	case errors.Is(err, ack.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ack.ErrRunActive):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"path": res.Path})
	}
}

// listPending 处理 GET {p}/confirmations?threadId=：
// 返回该会话的待确认项（含重启后的孤儿记录，live=false）。
func (h *handlers) listPending(w http.ResponseWriter, r *http.Request) {
	threadID := r.URL.Query().Get("threadId")
	if threadID == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing threadId"))
		return
	}
	items, err := h.gw.Pending(r.Context(), threadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []ack.PendingItem{}
	}
	writeJSON(w, http.StatusOK, items)
}

// writeJSON 以 UTF-8 JSON 写出响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 以 JSON 写出错误响应。
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
