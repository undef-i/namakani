package api

import (
	"net/http"
	"strconv"
	"strings"

	"namakani/internal/auth"
)

// Access tokens exist only for development: a CLI presents one to wake a
// dev instance and debug it. They are created, listed and revoked from the
// settings page, which requires a real login session: a token must never be
// able to manage tokens.

// sessionOnly guards handlers that require a session cookie specifically,
// rejecting requests authenticated by an access token.
func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.sessionKey(r); !ok {
			writeErr(w, http.StatusUnauthorized, "尚未登录")
			return
		}
		s.renewSession(w, r)
		next(w, r)
	}
}

// handleRevokeAllSessions deletes every session row, the caller's
// included, and drops the in-memory session keys with them. Access
// tokens are a separate table and keep working. With no live session
// left, workers stop, exactly as on the last logout.
func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	n, err := auth.DeleteAllSessions(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "吊销会话失败")
		return
	}
	s.mu.Lock()
	for tok := range s.keys {
		if !strings.HasPrefix(tok, "bearer:") {
			delete(s.keys, tok)
		}
	}
	s.mu.Unlock()
	s.mgr.StopAll()
	http.SetCookie(w, &http.Cookie{Name: "namakani_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}

type accessTokenOut struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

func (s *Server) handleCreateAccessToken(w http.ResponseWriter, r *http.Request) {
	key, ok := s.sessionKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "尚未登录")
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeErr(w, http.StatusBadRequest, "请填写令牌名称")
		return
	}
	if len([]rune(label)) > 64 {
		writeErr(w, http.StatusBadRequest, "令牌名称太长")
		return
	}
	wrapped := ""
	if s.serverKey != nil {
		sealed, err := auth.Encrypt(s.serverKey, string(key))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "创建令牌失败")
			return
		}
		wrapped = sealed
	}
	id, token, err := auth.NewAccessToken(s.st, label, wrapped)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "创建令牌失败")
		return
	}
	// The plaintext token is returned exactly once, in this response.
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"label": label,
		"token": token,
	})
}

func (s *Server) handleListAccessTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := auth.ListAccessTokens(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取令牌失败")
		return
	}
	out := make([]accessTokenOut, 0, len(rows))
	for _, tk := range rows {
		out = append(out, accessTokenOut{ID: tk.ID, Label: tk.Label, CreatedAt: tk.CreatedAt, LastUsedAt: tk.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleDeleteAccessToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "令牌编号不对")
		return
	}
	deleted, err := auth.DeleteAccessToken(s.st, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "吊销令牌失败")
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "令牌不存在")
		return
	}
	// The in-memory key and running workers are deliberately left alone,
	// mirroring a single-session logout: the row was the token's only
	// authority, so its next request fails on its own.
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
