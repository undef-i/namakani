package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"namakani/internal/avatar"
)

func (s *Server) resolveAvatar(ctx context.Context, email string) (string, string, []byte) {
	if s.avatarResolve != nil {
		return s.avatarResolve(ctx, email)
	}
	return avatar.DefaultResolver().Resolve(ctx, email)
}

func (s *Server) handleAvatars(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Emails []string `json:"emails"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if len(req.Emails) > 60 {
		req.Emails = req.Emails[:60]
	}
	now := time.Now().Unix()
	results := make(map[string]string, len(req.Emails))
	var todo []string
	seen := map[string]bool{}
	for _, e := range req.Emails {
		norm := avatar.NormalizeEmail(e)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		if row, ok, err := s.st.GetAvatar(norm); err == nil && ok {
			if row.Kind != "" || now-row.CheckedAt < int64(avatarNegativeTTL/time.Second) {
				results[norm] = row.Kind
				continue
			}
		}
		todo = append(todo, norm)
	}
	if len(todo) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), avatarResolveWindow)
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, email := range todo {
			wg.Add(1)
			go func(email string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				kind, mime, data := s.resolveAvatar(ctx, email)
				_ = s.st.PutAvatar(email, kind, mime, data, now)
				mu.Lock()
				results[email] = kind
				mu.Unlock()
			}(email)
		}
		wg.Wait()
		cancel()
	}
	for _, email := range todo {
		if _, ok := results[email]; !ok {
			results[email] = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"avatars": results})
}

func (s *Server) handleAvatarImage(w http.ResponseWriter, r *http.Request) {
	email := avatar.NormalizeEmail(r.URL.Query().Get("e"))
	if email == "" {
		writeErr(w, http.StatusBadRequest, "请填写邮箱")
		return
	}
	row, ok, err := s.st.GetAvatar(email)
	if err != nil || !ok || row.Kind == "" || len(row.Data) == 0 {
		writeErr(w, http.StatusNotFound, "没有头像")
		return
	}
	mime := row.Mime
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(row.Data)
}
