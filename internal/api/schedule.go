package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"namakani/internal/auth"
	"namakani/internal/mail"
	"namakani/internal/proton"
	"namakani/internal/tuta"

	"github.com/emersion/go-imap/v2"
)

type scheduledAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	File        string `json:"file"`
}

type scheduledPayload struct {
	Req         composeReq            `json:"req"`
	Attachments []scheduledAttachment `json:"attachments"`
}

type scheduleReq struct {
	composeReq
	SendAt int64 `json:"send_at"`
}

func (s *Server) scheduledDir(id int64) string {
	return filepath.Join(s.st.DataDir(), "scheduled", strconv.FormatInt(id, 10))
}

func (s *Server) handleScheduleSend(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req scheduleReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.SendAt <= 0 {
		writeErr(w, http.StatusBadRequest, "send_at required")
		return
	}
	if _, err := s.prepareOutgoing(key, req.composeReq, true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.st.DB.Exec(`INSERT INTO scheduled_sends(account_id, payload, send_at, created_at, status)
		VALUES(?, '', ?, ?, 'pending')`, req.AccountID, req.SendAt, time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "schedule failed")
		return
	}
	id, _ := res.LastInsertId()
	fail := func(msg string) {
		_, _ = s.st.DB.Exec("DELETE FROM scheduled_sends WHERE id = ?", id)
		_ = os.RemoveAll(s.scheduledDir(id))
		writeErr(w, http.StatusInternalServerError, msg)
	}
	dir := s.scheduledDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail("schedule failed")
		return
	}
	sp := scheduledPayload{Req: req.composeReq}
	sp.Req.Attachments = nil
	for i, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataB64)
		if err != nil {
			fail("附件 " + a.Filename + " 的数据无效")
			return
		}
		name := "att" + strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			fail("schedule failed")
			return
		}
		sp.Attachments = append(sp.Attachments, scheduledAttachment{
			Filename: a.Filename, ContentType: a.ContentType, File: name,
		})
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		fail("schedule failed")
		return
	}
	if _, err := s.st.DB.Exec("UPDATE scheduled_sends SET payload = ? WHERE id = ?", string(raw), id); err != nil {
		fail("schedule failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

type scheduledOut struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Subject   string `json:"subject"`
	To        string `json:"to"`
	SendAt    int64  `json:"send_at"`
	CreatedAt int64  `json:"created_at"`
	Status    string `json:"status"`
	Error     string `json:"error"`
}

func (s *Server) handleListScheduled(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT id, account_id, payload, send_at, created_at, status, error
		FROM scheduled_sends ORDER BY send_at DESC LIMIT 100`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	defer rows.Close()
	out := []scheduledOut{}
	for rows.Next() {
		var o scheduledOut
		var payload string
		if err := rows.Scan(&o.ID, &o.AccountID, &payload, &o.SendAt, &o.CreatedAt, &o.Status, &o.Error); err != nil {
			writeErr(w, http.StatusInternalServerError, "list failed")
			return
		}
		var sp scheduledPayload
		if err := json.Unmarshal([]byte(payload), &sp); err == nil {
			o.Subject = sp.Req.Subject
			o.To = sp.Req.To
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCancelScheduled(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var status string
	if err := s.st.DB.QueryRow("SELECT status FROM scheduled_sends WHERE id = ?", id).Scan(&status); err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if status != "pending" {
		writeErr(w, http.StatusConflict, "already "+status)
		return
	}
	if _, err := s.st.DB.Exec("DELETE FROM scheduled_sends WHERE id = ?", id); err != nil {
		writeErr(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	_ = os.RemoveAll(s.scheduledDir(id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) anyCredentialKey() ([]byte, bool) {
	s.mu.Lock()
	for _, k := range s.keys {
		s.mu.Unlock()
		return k, true
	}
	s.mu.Unlock()
	if s.serverKey == nil {
		return nil, false
	}
	rows, err := s.st.DB.Query("SELECT wrapped_key, expires_at FROM sessions WHERE wrapped_key != '' ORDER BY expires_at DESC")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	now := time.Now().Unix()
	for rows.Next() {
		var wrapped string
		var expiresAt int64
		if err := rows.Scan(&wrapped, &expiresAt); err != nil || expiresAt <= now {
			continue
		}
		plain, err := auth.Decrypt(s.serverKey, wrapped)
		if err == nil {
			return []byte(plain), true
		}
	}
	return nil, false
}

func (s *Server) scheduleLoop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for range t.C {
		s.runDueScheduled()
	}
}

func (s *Server) runDueScheduled() {
	key, ok := s.anyCredentialKey()
	if !ok {
		return
	}
	rows, err := s.st.DB.Query(`SELECT id FROM scheduled_sends
		WHERE status = 'pending' AND send_at <= ? ORDER BY send_at LIMIT 10`, time.Now().Unix())
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.fireScheduled(key, id)
	}
}

func (s *Server) fireScheduled(key []byte, id int64) {
	var payload string
	var status string
	if err := s.st.DB.QueryRow("SELECT payload, status FROM scheduled_sends WHERE id = ?", id).
		Scan(&payload, &status); err != nil || status != "pending" {
		return
	}
	mark := func(st, errText string) {
		_, _ = s.st.DB.Exec("UPDATE scheduled_sends SET status = ?, error = ? WHERE id = ?", st, errText, id)
		_ = os.RemoveAll(s.scheduledDir(id))
	}
	var sp scheduledPayload
	if err := json.Unmarshal([]byte(payload), &sp); err != nil {
		mark("failed", "payload invalid")
		return
	}
	req := sp.Req
	for _, a := range sp.Attachments {
		data, err := os.ReadFile(filepath.Join(s.scheduledDir(id), a.File))
		if err != nil {
			mark("failed", "attachment file missing")
			return
		}
		req.Attachments = append(req.Attachments, attachmentIn{
			Filename: a.Filename, ContentType: a.ContentType,
			DataB64: base64.StdEncoding.EncodeToString(data),
		})
	}
	out, err := s.prepareOutgoing(key, req, true)
	if err != nil {
		mark("failed", err.Error())
		return
	}
	var fileCopy func() (int64, error)
	if out.acct.Kind == "proton" {
		sess, err := proton.Connect(s.st, out.acct)
		if err != nil {
			mark("failed", err.Error())
			return
		}
		sentProtonID, err := sess.SendRaw(out.raw)
		if err != nil {
			mark("failed", err.Error())
			return
		}
		fileCopy = func() (int64, error) { return s.fileSentCopy(out.acct, sess, sentProtonID, out) }
	} else if out.acct.Kind == "tuta" {
		sess, err := tuta.Connect(s.st, out.acct)
		if err != nil {
			mark("failed", err.Error())
			return
		}
		sentTutaID, err := sess.SendRaw(out.raw)
		if err != nil {
			mark("failed", err.Error())
			return
		}
		fileCopy = func() (int64, error) { return s.fileSentCopy(out.acct, sess, sentTutaID, out) }
	} else {
		if err := mail.SendSMTP(out.acct, out.acct.Email, out.rcpts, out.raw); err != nil {
			mark("failed", err.Error())
			return
		}
		fileCopy = func() (int64, error) {
			return s.appendCopy(key, req.AccountID, "sent", []imap.Flag{imap.FlagSeen}, mail.FlagSeen, out)
		}
	}
	note := ""
	if sentID, err := fileCopy(); err != nil {
		note = "邮件已发送，但存入「已发送」失败：" + err.Error()
	} else {
		s.broadcastMessages([]int64{sentID})
		if fid, _, ok, _ := s.st.FolderBySpecial(req.AccountID, "sent"); ok {
			s.broadcastFolder(fid)
		}
	}
	if req.DraftID > 0 {
		_ = s.deleteMessageEverywhere(key, req.DraftID)
	}
	mark("sent", note)
}
