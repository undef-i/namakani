package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"namakani/internal/store"
)

func outgoingDir(dataDir string, opID int64) string {
	return filepath.Join(dataDir, "outgoing", strconv.FormatInt(opID, 10))
}

func (s *Server) discardSendPayload(o *store.Op) {
	_ = os.RemoveAll(outgoingDir(s.st.DataDir(), o.ID))
}

func (s *Server) broadcastSendState(opID int64, status string, sent bool, undoIgnored bool) {
	s.hub.broadcast(map[string]any{
		"type": "ops", "op_id": opID, "op_type": store.OpSend,
		"status": status, "sent": sent, "undo_ignored": undoIgnored,
	})
}

func (s *Server) broadcastOpApplied(o *store.Op) {
	switch o.Type {
	case store.OpFlags:
		s.broadcastFlags([]int64{o.MessageID})
		if route, err := s.st.MessageRoute(o.MessageID); err == nil {
			s.broadcastFolder(route.FolderID)
		}
	case store.OpMove:
		s.broadcastRemoved(o.PriorFolderID, []int64{o.MessageID})
		s.broadcastFolder(o.PriorFolderID)
		s.broadcastFolder(o.ToFolderID)
		s.broadcastMessages([]int64{o.MessageID})
	case store.OpExpunge:
		s.broadcastRemoved(o.PriorFolderID, []int64{o.MessageID})
		s.broadcastFolder(o.PriorFolderID)
	}
}

func (s *Server) broadcastOpReverted(o *store.Op) {
	switch o.Type {
	case store.OpFlags:
		s.broadcastFlags([]int64{o.MessageID})
		if route, err := s.st.MessageRoute(o.MessageID); err == nil {
			s.broadcastFolder(route.FolderID)
		}
	case store.OpMove:
		s.broadcastRemoved(o.ToFolderID, []int64{o.MessageID})
		s.broadcastFolder(o.ToFolderID)
		s.broadcastFolder(o.PriorFolderID)
		s.broadcastMessages([]int64{o.MessageID})
	case store.OpExpunge:
		s.broadcastMessages([]int64{o.MessageID})
		s.broadcastFolder(o.PriorFolderID)
	}
}

func cannotUndoText(o *store.Op) string {
	switch o.Type {
	case store.OpExpunge:
		return "这封邮件已经被彻底删除，无法撤销"
	case store.OpSend:
		return "这封邮件已经发出，无法撤销"
	}
	return "这条操作已经完成，无法撤销"
}

func (s *Server) writeOpStateErr(w http.ResponseWriter, code int, msg string, opID int64) {
	status := "gone"
	if o, err := s.st.GetOp(opID); err == nil && o != nil {
		status = o.Status
	}
	writeJSON(w, code, map[string]any{"error": msg, "op_status": status})
}

func (s *Server) handleUndoOp(w http.ResponseWriter, r *http.Request) {
	opID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "操作编号不对")
		return
	}
	before, err := s.st.GetOp(opID)
	if err != nil {
		s.writeOpStateErr(w, http.StatusNotFound, "操作不存在", opID)
		return
	}
	if before.Type == store.OpSend {
		var sp sendOpPayload
		if err := json.Unmarshal([]byte(before.Payload), &sp); err == nil && sp.Sent {
			s.broadcastSendState(before.ID, store.OpDone, true, false)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "already_sent"})
			return
		}
	}
	res, err := s.st.UndoOp(opID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUndoTargetGone):
			s.writeOpStateErr(w, http.StatusConflict, "这封邮件已不存在，无法撤销", opID)
		case errors.Is(err, store.ErrOpCannotUndo):
			s.writeOpStateErr(w, http.StatusConflict, cannotUndoText(before), opID)
		case errors.Is(err, store.ErrOpSuperseded):
			s.writeOpStateErr(w, http.StatusConflict, "这条操作后面还有更新的操作，请先撤销最新的一条", opID)
		default:
			writeErr(w, http.StatusInternalServerError, "撤销失败")
		}
		return
	}
	if res.Mode == "already_sent" {
		s.broadcastSendState(before.ID, store.OpDone, true, false)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "already_sent"})
		return
	}
	if res.Mode == "cancelled" {
		s.broadcastOpReverted(before)
		if before.Type == store.OpSend {
			s.discardSendPayload(before)
		}
	} else if res.Inverse != nil {
		s.broadcastOpApplied(res.Inverse)
	}
	s.broadcastOps()
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": res.Mode})
}

type attentionOut struct {
	OpID         int64  `json:"op_id"`
	AccountID    int64  `json:"account_id"`
	AccountLabel string `json:"account_label"`
	Type         string `json:"type"`
	MessageID    int64  `json:"message_id"`
	Subject      string `json:"subject"`
	To           string `json:"to"`
	Error        string `json:"error"`
	CreatedAt    int64  `json:"created_at"`
	Attempts     int    `json:"attempts"`
}

// resolveDisplayTarget fills a listed op's message coordinate and subject
// from the message's current incarnation, resolved by durable identity, so
// a folder rebuild never leaves the panel pointing at a dead row.
func (s *Server) resolveDisplayTarget(o *store.Op) (int64, string) {
	t, ok, err := s.st.ResolveOpTarget(o)
	if err != nil || !ok {
		return o.MessageID, ""
	}
	var subj string
	if err := s.st.DB.QueryRow("SELECT subject FROM messages WHERE id = ?", t.RowID).Scan(&subj); err != nil {
		return t.RowID, ""
	}
	return t.RowID, subj
}

func (s *Server) handleListAttention(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT o.id, o.account_id, a.label, a.email, o.type, o.message_id,
		COALESCE(m.subject, ''), o.payload, o.last_error, o.created_at, o.attempts, o.identity, o.prior_folder_id
		FROM ops o
		JOIN accounts a ON a.id = o.account_id
		LEFT JOIN messages m ON m.id = o.message_id
		WHERE o.status = 'needs_attention' ORDER BY o.created_at ASC LIMIT 200`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "列表加载失败")
		return
	}
	out := []attentionOut{}
	var bound []*store.Op
	for rows.Next() {
		var it attentionOut
		var email, payload string
		var o store.Op
		if err := rows.Scan(&it.OpID, &it.AccountID, &it.AccountLabel, &email, &it.Type, &it.MessageID,
			&it.Subject, &payload, &it.Error, &it.CreatedAt, &it.Attempts, &o.Identity, &o.PriorFolderID); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, "列表加载失败")
			return
		}
		if it.AccountLabel == "" {
			it.AccountLabel = email
		}
		if it.Type == store.OpSend {
			var sp sendOpPayload
			if err := json.Unmarshal([]byte(payload), &sp); err == nil {
				it.Subject = sp.Req.Subject
				it.To = sp.Req.To
			}
		}
		o.ID, o.AccountID, o.MessageID = it.OpID, it.AccountID, it.MessageID
		out = append(out, it)
		bound = append(bound, &o)
	}
	rows.Close()
	for i, o := range bound {
		if out[i].Type == store.OpSend || o.Identity == "" {
			continue
		}
		out[i].MessageID, out[i].Subject = s.resolveDisplayTarget(o)
	}
	writeJSON(w, http.StatusOK, out)
}

type pendingOut struct {
	OpID          int64  `json:"op_id"`
	ClientKey     string `json:"client_key"`
	AccountID     int64  `json:"account_id"`
	AccountLabel  string `json:"account_label"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	MessageID     int64  `json:"message_id"`
	Subject       string `json:"subject"`
	To            string `json:"to"`
	PriorFolderID int64  `json:"prior_folder_id"`
	PriorSpecial  string `json:"prior_special"`
	ToFolderID    int64  `json:"to_folder_id"`
	ToSpecial     string `json:"to_special"`
	NotBefore     int64  `json:"not_before"`
	Attempts      int    `json:"attempts"`
	NextAttempt   int64  `json:"next_attempt"`
	CreatedAt     int64  `json:"created_at"`
}

func (s *Server) handleListPending(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT o.id, o.client_key, o.account_id, a.label, a.email, o.type, o.status, o.message_id,
		COALESCE(m.subject, ''), o.payload, o.prior_folder_id, COALESCE(pf.special_use, ''),
		o.to_folder_id, COALESCE(tf.special_use, ''), o.not_before, o.attempts, o.next_attempt, o.created_at, o.identity
		FROM ops o
		JOIN accounts a ON a.id = o.account_id
		LEFT JOIN messages m ON m.id = o.message_id
		LEFT JOIN folders pf ON pf.id = o.prior_folder_id
		LEFT JOIN folders tf ON tf.id = o.to_folder_id
		WHERE o.status IN ('pending', 'in_flight') ORDER BY o.id ASC LIMIT 500`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "列表加载失败")
		return
	}
	out := []pendingOut{}
	var bound []*store.Op
	for rows.Next() {
		var it pendingOut
		var email, payload string
		var o store.Op
		if err := rows.Scan(&it.OpID, &it.ClientKey, &it.AccountID, &it.AccountLabel, &email, &it.Type, &it.Status, &it.MessageID,
			&it.Subject, &payload, &it.PriorFolderID, &it.PriorSpecial,
			&it.ToFolderID, &it.ToSpecial, &it.NotBefore, &it.Attempts, &it.NextAttempt, &it.CreatedAt, &o.Identity); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, "列表加载失败")
			return
		}
		if it.AccountLabel == "" {
			it.AccountLabel = email
		}
		if it.Type == store.OpSend {
			var sp sendOpPayload
			if err := json.Unmarshal([]byte(payload), &sp); err == nil {
				it.Subject = sp.Req.Subject
				it.To = sp.Req.To
			}
		}
		o.ID, o.AccountID, o.MessageID, o.PriorFolderID = it.OpID, it.AccountID, it.MessageID, it.PriorFolderID
		out = append(out, it)
		bound = append(bound, &o)
	}
	rows.Close()
	for i, o := range bound {
		if out[i].Type == store.OpSend || o.Identity == "" {
			continue
		}
		out[i].MessageID, out[i].Subject = s.resolveDisplayTarget(o)
	}
	writeJSON(w, http.StatusOK, out)
}

type byKeysReq struct {
	Keys []string `json:"keys"`
}

type byKeyOut struct {
	ClientKey string `json:"client_key"`
	OpID      int64  `json:"op_id"`
	Status    string `json:"status"`
}

func (s *Server) handleOpsByKeys(w http.ResponseWriter, r *http.Request) {
	var req byKeysReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	out := []byKeyOut{}
	for _, k := range req.Keys {
		if k == "" {
			continue
		}
		o, err := s.st.OpByClientKey(k)
		if err != nil || o == nil {
			continue
		}
		out = append(out, byKeyOut{ClientKey: k, OpID: o.ID, Status: o.Status})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRetryOp(w http.ResponseWriter, r *http.Request) {
	opID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "操作编号不对")
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil {
		s.writeOpStateErr(w, http.StatusNotFound, "操作不存在", opID)
		return
	}
	if err := s.st.RetryOp(opID); err != nil {
		s.writeOpStateErr(w, http.StatusConflict, "这条操作当前不需要重试", opID)
		return
	}
	_ = o
	s.broadcastOps()
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDiscardOp(w http.ResponseWriter, r *http.Request) {
	opID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "操作编号不对")
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil {
		s.writeOpStateErr(w, http.StatusNotFound, "操作不存在", opID)
		return
	}
	if err := s.st.CancelOp(opID); err != nil {
		s.writeOpStateErr(w, http.StatusConflict, "这条操作已经执行完了，无法丢弃", opID)
		return
	}
	s.broadcastOpReverted(o)
	if o.Type == store.OpSend {
		s.discardSendPayload(o)
	}
	s.broadcastOps()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
