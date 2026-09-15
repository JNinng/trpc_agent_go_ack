package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// registerSessionAPI 挂载示例级的会话列表/删除路由。会话管理属库的
// 范围外（session.Service 已提供能力），此处给出侧栏所需的参考实现。
func registerSessionAPI(mux *http.ServeMux, svc session.Service) {
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		sessions, err := svc.ListSessions(r.Context(), session.UserKey{AppName: AppName, UserID: UserID})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items := make([]sessionItem, 0, len(sessions))
		for _, s := range sessions {
			items = append(items, sessionItem{
				ID:        s.ID,
				Title:     sessionTitle(s),
				CreatedAt: s.CreatedAt.Format("2006-01-02 15:04:05"),
				UpdatedAt: s.UpdatedAt.Format("2006-01-02 15:04:05"),
			})
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("missing session id"))
			return
		}
		if err := svc.DeleteSession(r.Context(), session.Key{AppName: AppName, UserID: UserID, SessionID: id}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// sessionItem 是会话列表接口的响应条目。
type sessionItem struct {
	ID        string `json:"id"`        // 会话 ID
	Title     string `json:"title"`     // 由首条用户消息派生的标题
	CreatedAt string `json:"createdAt"` // 创建时间（格式化后的字符串）
	UpdatedAt string `json:"updatedAt"` // 最近更新时间
}

// sessionTitle 从会话的第一条用户消息派生侧栏标题。
func sessionTitle(s *session.Session) string {
	for i := range s.Events {
		ev := &s.Events[i]
		if ev.Response == nil {
			continue
		}
		for _, c := range ev.Response.Choices {
			if c.Message.Role == model.RoleUser && strings.TrimSpace(c.Message.Content) != "" {
				return truncate(strings.TrimSpace(c.Message.Content), 30)
			}
		}
	}
	return "（新会话）"
}

// truncate 按 rune 截断字符串，超长时以省略号结尾。
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
