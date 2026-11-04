package api

import (
	"net/http"
	"strconv"
	"strings"

	"namakani/internal/auth"
)

func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.sessionKey(r); !ok {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		s.renewSession(w, r)
		next(w, r)
	}
}

func (s *Server) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	n, err := auth.DeleteAllSessions(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revoke the session")
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
	http.SetCookie(w, &http.Cookie{Name: "namakani_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure(r), SameSite: http.SameSiteLaxMode})
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
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeErr(w, http.StatusBadRequest, "token name is required")
		return
	}
	if len([]rune(label)) > 64 {
		writeErr(w, http.StatusBadRequest, "token name is too long")
		return
	}
	wrapped := ""
	if s.serverKey != nil {
		sealed, err := auth.Encrypt(s.serverKey, string(key))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to create the token")
			return
		}
		wrapped = sealed
	}
	id, token, err := auth.NewAccessToken(s.st, label, wrapped)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    id,
		"label": label,
		"token": token,
	})
}

func (s *Server) handleListAccessTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := auth.ListAccessTokens(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the token")
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
		writeErr(w, http.StatusBadRequest, "invalid token id")
		return
	}
	deleted, err := auth.DeleteAccessToken(s.st, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to revoke the token")
		return
	}
	if !deleted {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
