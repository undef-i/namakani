package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	syncmgr "namakani/internal/sync"
)

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan []byte]struct{})}
}

func (h *hub) subscribe() (chan []byte, func()) {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *hub) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- data:
		default:
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := s.hub.subscribe()
	defer unsub()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) broadcastMessages(ids []int64) {
	if len(ids) == 0 {
		return
	}
	rows := s.messageRowsByIDs(ids)
	if len(rows) > 0 {
		s.hub.broadcast(map[string]any{"type": "messages", "items": rows})
	}
}

func (s *Server) broadcastFlags(ids []int64) {
	if len(ids) == 0 {
		return
	}
	items := []map[string]any{}
	for _, id := range ids {
		flags, err := s.st.MessageFlags(id)
		if err != nil {
			continue
		}
		items = append(items, map[string]any{"id": id, "flags": flags})
	}
	if len(items) > 0 {
		s.hub.broadcast(map[string]any{"type": "flags", "items": items})
	}
}

func (s *Server) broadcastRemoved(folderID int64, ids []int64) {
	if len(ids) == 0 {
		return
	}
	s.hub.broadcast(map[string]any{"type": "removed", "folder_id": folderID, "ids": ids})
}

func (s *Server) broadcastFolder(folderID int64) {
	f, ok := s.folderRow(folderID)
	if !ok {
		return
	}
	s.hub.broadcast(map[string]any{"type": "folder", "folder": f})
}

func (s *Server) folderRow(folderID int64) (folderOut, bool) {
	var f folderOut
	err := s.st.DB.QueryRow(`SELECT f.id, f.account_id, a.label, f.name, f.special_use, f.total, f.unread
		FROM folders f JOIN accounts a ON a.id = f.account_id WHERE f.id = ?`, folderID).
		Scan(&f.ID, &f.AccountID, &f.AccountLabel, &f.Name, &f.SpecialUse, &f.Total, &f.Unread)
	return f, err == nil
}

func (s *Server) onSyncChange(c syncmgr.Change) {
	s.broadcastMessages(c.Inserted)
	s.broadcastFlags(c.Flagged)
	s.broadcastRemoved(c.FolderID, c.Removed)
	s.broadcastFolder(c.FolderID)
	s.tgNotify(c)
	s.wpNotify(c)
}
