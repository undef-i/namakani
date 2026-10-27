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
	seq  int64
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
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := v.(map[string]any); ok {
		h.seq++
		m["seq"] = h.seq
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	for ch := range h.subs {
		select {
		case ch <- data:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "服务器不支持流式推送")
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
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

type placedRow struct {
	messageOut
	Pv int64 `json:"pv"`
}

func (s *Server) broadcastMessages(ids []int64) {
	if len(ids) == 0 {
		return
	}
	if pending := s.st.PendingExpungeSet(ids); len(pending) > 0 {
		keep := make([]int64, 0, len(ids))
		for _, id := range ids {
			if !pending[id] {
				keep = append(keep, id)
			}
		}
		ids = keep
	}
	if len(ids) == 0 {
		return
	}
	rows := s.messageRowsByIDs(ids)
	if len(rows) > 0 {
		pvs, _ := s.st.PlacementVersions(ids)
		items := make([]placedRow, 0, len(rows))
		for _, r := range rows {
			items = append(items, placedRow{r, pvs[r.ID]})
		}
		s.hub.broadcast(map[string]any{"type": "messages", "items": items})
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
	pvs, _ := s.st.PlacementVersions(ids)
	s.hub.broadcast(map[string]any{"type": "removed", "folder_id": folderID, "ids": ids, "pvs": pvs})
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
	for _, cr := range c.CrossRemoved {
		s.broadcastRemoved(cr.FolderID, cr.IDs)
		s.broadcastFolder(cr.FolderID)
	}
	s.broadcastFolder(c.FolderID)
	s.broadcastOps()
	s.tgNotify(c)
	s.wpNotify(c)
}
