package api

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"namakani/internal/mail"
	"namakani/internal/gmail"
	"namakani/internal/proton"
	"namakani/internal/store"
	"namakani/internal/tuta"

	"github.com/emersion/go-imap/v2"
)

type sendOpAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	File        string `json:"file"`
}

type sendOpPayload struct {
	Req         composeReq         `json:"req"`
	Attachments []sendOpAttachment `json:"attachments"`
	Sent        bool               `json:"sent"`
	SentID      string             `json:"sent_id"`
	RawB64      string             `json:"raw_b64,omitempty"`
	MessageID   string             `json:"message_id,omitempty"`
	DraftIdentity string `json:"draft_identity,omitempty"`
}

type sendReq struct {
	composeReq
	UndoSeconds int    `json:"undo_seconds"`
	ClientKey   string `json:"client_key"`
}

type scheduleReq struct {
	composeReq
	SendAt    int64  `json:"send_at"`
	ClientKey string `json:"client_key"`
}

func (s *Server) draftIdentity(accountID, draftID int64) string {
	if identity, err := s.st.IdentityForRow(accountID, draftID); err == nil && identity != "" {
		return identity
	}
	if identity, err := s.st.PriorSendDraftIdentity(accountID, draftID); err == nil {
		return identity
	}
	return ""
}

func (s *Server) enqueueSendOp(key []byte, req composeReq, notBefore int64, clientKey string) (int64, error) {
	if _, err := s.prepareOutgoing(key, req, true); err != nil {
		return 0, err
	}
	sp := sendOpPayload{Req: req}
	sp.Req.Attachments = nil
	if req.DraftID > 0 {
		sp.DraftIdentity = s.draftIdentity(req.AccountID, req.DraftID)
	}
	type decoded struct {
		att  sendOpAttachment
		data []byte
	}
	var files []decoded
	for i, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataB64)
		if err != nil {
			return 0, errInvalidAttachment(a.Filename)
		}
		name := "att" + strconv.Itoa(i)
		files = append(files, decoded{sendOpAttachment{Filename: a.Filename, ContentType: a.ContentType, File: name}, data})
		sp.Attachments = append(sp.Attachments, sendOpAttachment{Filename: a.Filename, ContentType: a.ContentType, File: name})
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		return 0, err
	}
	opID, err := s.st.EnqueueSend(req.AccountID, string(raw), notBefore, clientKey)
	if err != nil {
		return 0, err
	}
	dir := outgoingDir(s.st.DataDir(), opID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = s.st.CancelOp(opID)
		return 0, err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.att.File), f.data, 0o600); err != nil {
			_ = s.st.CancelOp(opID)
			_ = os.RemoveAll(dir)
			return 0, err
		}
	}
	s.broadcastOps()
	return opID, nil
}

func errInvalidAttachment(name string) error {
	return &attachmentError{name: name}
}

type attachmentError struct{ name string }

func (e *attachmentError) Error() string { return "附件 " + e.name + " 的数据无效" }

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req sendReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	undo := req.UndoSeconds
	if undo < 0 {
		undo = 0
	}
	if undo > 120 {
		undo = 120
	}
	notBefore := time.Now().Unix() + int64(undo)
	opID, err := s.enqueueSendOp(key, req.composeReq, notBefore, req.ClientKey)
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "发送失败，请稍后重试", err)
		return
	}
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID, "not_before": notBefore})
}

func (s *Server) deliverOutgoing(key []byte, out *outgoing) (string, error) {
	if s.sendDeliver != nil {
		return s.sendDeliver(key, out)
	}
	switch out.acct.Kind {
	case "proton":
		sess, err := proton.Connect(s.st, out.acct)
		if err != nil {
			return "", err
		}
		return sess.SendRaw(out.raw)
	case "tuta":
		sess, err := tuta.Connect(s.st, out.acct)
		if err != nil {
			return "", err
		}
		return sess.SendRaw(out.raw)
	case "gmail":
		sess, err := gmail.Connect(s.st, out.acct)
		if err != nil {
			return "", err
		}
		return sess.SendRaw(out.raw)
	}
	if err := mail.SendSMTP(out.acct, out.acct.Email, out.rcpts, out.raw); err != nil {
		return "", err
	}
	return "", nil
}

func (s *Server) fileOutgoingCopy(key []byte, out *outgoing, sentID string) (int64, error) {
	if s.sendFileCopy != nil {
		return s.sendFileCopy(key, out, sentID)
	}
	if _, _, ok, err := s.st.FolderBySpecial(out.acct.ID, "sent"); err != nil {
		return 0, err
	} else if !ok {
		return 0, nil
	}
	switch out.acct.Kind {
	case "proton":
		sess, err := proton.Connect(s.st, out.acct)
		if err != nil {
			return 0, err
		}
		return s.fileSentCopy(out.acct, sess, sentID, out)
	case "tuta":
		sess, err := tuta.Connect(s.st, out.acct)
		if err != nil {
			return 0, err
		}
		return s.fileSentCopy(out.acct, sess, sentID, out)
	case "gmail":
		sess, err := gmail.Connect(s.st, out.acct)
		if err != nil {
			return 0, err
		}
		return s.fileSentCopy(out.acct, sess, sentID, out)
	}
	return s.appendCopy(key, out.acct.ID, "sent", []imap.Flag{imap.FlagSeen}, mail.FlagSeen, out)
}

func (s *Server) executeSendWave(key []byte, wave *drainWave, ops []*store.Op) (int, []int64) {
	done := 0
	var failed []int64
	for _, o := range ops {
		if s.executeSend(key, o) {
			done++
			wave.bump()
			continue
		}
		if cur, err := s.st.GetOp(o.ID); err == nil && cur.Status == store.OpPending {
			failed = append(failed, o.ID)
		}
		wave.bump()
	}
	return done, failed
}

func (s *Server) executeSend(key []byte, o *store.Op) bool {
	now := time.Now().Unix()
	sentSoFar := false
	fail := func(err error, perm bool) {
		if !s.opStillOurs(o) {
			return
		}
		status, ferr := s.st.FailOp(o.ID, err.Error(), perm, now)
		if ferr == nil && status == store.OpNeedsAttention {
			s.broadcastOps()
			s.broadcastSendState(o.ID, store.OpNeedsAttention, sentSoFar, false)
			s.broadcastScheduled()
		}
	}
	var sp sendOpPayload
	if err := json.Unmarshal([]byte(o.Payload), &sp); err != nil {
		fail(err, true)
		return false
	}
	sentSoFar = sp.Sent
	req := sp.Req
	for _, a := range sp.Attachments {
		data, err := os.ReadFile(filepath.Join(outgoingDir(s.st.DataDir(), o.ID), a.File))
		if err != nil {
			fail(errAttachmentMissing, true)
			return false
		}
		req.Attachments = append(req.Attachments, attachmentIn{
			Filename: a.Filename, ContentType: a.ContentType,
			DataB64: base64.StdEncoding.EncodeToString(data),
		})
	}
	out, err := s.prepareOutgoing(key, req, true)
	if err != nil {
		fail(err, true)
		return false
	}
	if sp.RawB64 == "" {
		sp.RawB64 = base64.StdEncoding.EncodeToString(out.raw)
		sp.MessageID = out.messageID
		raw, merr := json.Marshal(sp)
		if merr != nil {
			fail(merr, false)
			return false
		}
		applied, err := s.st.UpdateOpPayload(o.ID, string(raw))
		if err != nil {
			fail(err, false)
			return false
		}
		if !applied {
			return false
		}
	} else {
		rawBytes, derr := base64.StdEncoding.DecodeString(sp.RawB64)
		if derr != nil {
			fail(derr, true)
			return false
		}
		out.raw = rawBytes
		out.messageID = sp.MessageID
	}
	if !sp.Sent {
		newToken := s.drainTokenSeq.Add(1)
		sealed, undoWon, serr := s.st.SealSendForDelivery(o.ID, o.ClaimToken, newToken)
		if serr != nil {
			fail(serr, false)
			return false
		}
		if !sealed {
			if undoWon {
				cerr := s.st.CancelInFlightSend(o.ID)
				if cerr != nil {
					cerr = s.st.CancelOp(o.ID)
				}
				if cerr == nil {
					if cur, gerr := s.st.GetOp(o.ID); gerr == nil && cur.ReturnToDraft {
						if _, merr := s.materializeScheduledDraft(key, o, &sp); merr != nil {
							log.Printf("namakani: send op %d: return cancelled letter to drafts: %v", o.ID, merr)
						}
					}
					_ = os.RemoveAll(outgoingDir(s.st.DataDir(), o.ID))
					s.broadcastSendState(o.ID, store.OpCancelled, false, false)
					s.broadcastScheduled()
					return true
				}
			}
			return false
		}
		o.ClaimToken = newToken
		s.broadcastSendState(o.ID, "sealed", false, false)
	}
	if !sp.Sent {
		sentID, err := s.deliverOutgoing(key, out)
		if err != nil {
			perm, _ := classifyOpError(err)
			fail(err, perm)
			return false
		}
		sp.Sent = true
		sentSoFar = true
		sp.SentID = sentID
		raw, merr := json.Marshal(sp)
		if merr != nil {
			fail(merr, false)
			return false
		}
		applied, err := s.st.UpdateOpPayload(o.ID, string(raw))
		if err != nil {
			fail(err, false)
			return false
		}
		if !applied {
			return false
		}
	}
	sentMsgID, err := s.fileOutgoingCopy(key, out, sp.SentID)
	if err != nil {
		fail(err, false)
		return false
	}
	s.consumeSendDraft(o.AccountID, sp.DraftIdentity, sp.Req.DraftID)
	undoIgnored := false
	if cur, gerr := s.st.GetOp(o.ID); gerr == nil && cur.UndoRequested {
		undoIgnored = true
		_ = s.st.ClearUndoRequest(o.ID)
	}
	if !s.opStillOurs(o) {
		return false
	}
	if err := s.st.CompleteOp(o.ID); err != nil {
		log.Printf("namakani: send op %d: complete: %v", o.ID, err)
		return false
	}
	_ = os.RemoveAll(outgoingDir(s.st.DataDir(), o.ID))
	if sentMsgID > 0 {
		s.broadcastMessages([]int64{sentMsgID})
		if fid, _, ok, _ := s.st.FolderBySpecial(o.AccountID, "sent"); ok {
			s.broadcastFolder(fid)
		}
	}
	s.broadcastSendState(o.ID, store.OpDone, true, undoIgnored)
	s.broadcastScheduled()
	return true
}

func (s *Server) consumeSendDraft(accountID int64, draftIdentity string, draftIDHint int64) {
	if draftIdentity == "" {
		return
	}
	t, ok, rerr := s.st.ResolveIdentityRow(accountID, draftIdentity, 0, draftIDHint)
	if rerr != nil || !ok {
		return
	}
	draftOpID, err := s.st.EnqueueExpunge(accountID, t.RowID, "")
	if err != nil {
		return
	}
	if dop, gerr := s.st.GetOp(draftOpID); gerr == nil {
		s.broadcastOpApplied(dop)
	}
}

var errAttachmentMissing = &attachmentMissingError{}

type attachmentMissingError struct{}

func (*attachmentMissingError) Error() string { return "附件文件缺失，请重新编写这封邮件再发送" }

func (s *Server) handleScheduleSend(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req scheduleReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if req.SendAt <= 0 {
		writeErr(w, http.StatusBadRequest, "请填写发送时间")
		return
	}
	clientKey := req.ClientKey
	if req.DraftID > 0 {
		if identity := s.draftIdentity(req.AccountID, req.DraftID); identity != "" {
			clientKey = "sched:" + identity
		}
	}
	opID, err := s.enqueueSendOp(key, req.composeReq, req.SendAt, clientKey)
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "发送失败，请稍后重试", err)
		return
	}
	if req.DraftID > 0 {
		if o, gerr := s.st.GetOp(opID); gerr == nil {
			var sp sendOpPayload
			if json.Unmarshal([]byte(o.Payload), &sp) == nil {
				s.consumeSendDraft(o.AccountID, sp.DraftIdentity, sp.Req.DraftID)
			}
		}
	}
	s.broadcastScheduled()
	s.kickDrain()
	writeJSON(w, http.StatusCreated, map[string]int64{"id": -opID})
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
	rows, err := s.st.DB.Query(`SELECT id, account_id, payload, not_before, created_at, status, last_error
		FROM ops WHERE type = 'send' AND status IN ('pending', 'in_flight', 'needs_attention')
		ORDER BY not_before ASC LIMIT 100`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "列表加载失败")
		return
	}
	defer rows.Close()
	out := []scheduledOut{}
	for rows.Next() {
		var o scheduledOut
		var payload string
		var status string
		if err := rows.Scan(&o.ID, &o.AccountID, &payload, &o.SendAt, &o.CreatedAt, &status, &o.Error); err != nil {
			writeErr(w, http.StatusInternalServerError, "列表加载失败")
			return
		}
		o.ID = -o.ID
		switch status {
		case "needs_attention":
			o.Status = "failed"
		case "in_flight":
			o.Status = "in_flight"
		default:
			o.Status = "pending"
			o.Error = ""
		}
		var sp sendOpPayload
		if err := json.Unmarshal([]byte(payload), &sp); err == nil {
			o.Subject = sp.Req.Subject
			o.To = sp.Req.To
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) scheduledOpID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id >= 0 {
		writeErr(w, http.StatusBadRequest, "编号不对")
		return 0, false
	}
	return -id, true
}

func (s *Server) materializeScheduledDraft(key []byte, o *store.Op, sp *sendOpPayload) (int64, error) {
	req := sp.Req
	req.DraftID = 0
	req.Attachments = nil
	dir := outgoingDir(s.st.DataDir(), o.ID)
	for _, a := range sp.Attachments {
		data, err := os.ReadFile(filepath.Join(dir, a.File))
		if err != nil {
			return 0, err
		}
		req.Attachments = append(req.Attachments, attachmentIn{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			DataB64:     base64.StdEncoding.EncodeToString(data),
		})
	}
	out, err := s.prepareOutgoing(key, req, false)
	if err != nil {
		return 0, err
	}
	draftID, err := s.appendCopy(key, o.AccountID, "drafts",
		[]imap.Flag{imap.FlagDraft}, mail.FlagSeen|mail.FlagDraft, out)
	if err != nil {
		return 0, err
	}
	s.broadcastMessages([]int64{draftID})
	if fid, _, ok, _ := s.st.FolderBySpecial(o.AccountID, "drafts"); ok {
		s.broadcastFolder(fid)
	}
	return draftID, nil
}

func (s *Server) handleCancelScheduled(w http.ResponseWriter, r *http.Request) {
	key, ok := s.credentialKey(r)
	if !ok {
		key, ok = s.anyCredentialKey()
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "登录已过期，请重新登录")
		return
	}
	opID, ok := s.scheduledOpID(w, r)
	if !ok {
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil || o.Type != store.OpSend {
		writeErr(w, http.StatusNotFound, "不存在")
		return
	}
	var sp sendOpPayload
	if err := json.Unmarshal([]byte(o.Payload), &sp); err != nil {
		writeErr(w, http.StatusInternalServerError, "定时邮件内容读取失败")
		return
	}
	if o.Status == store.OpInFlight {
		purge := r.URL.Query().Get("purge") == "1"
		verb := "取消"
		if purge {
			verb = "删除"
		}
		if sp.Sent {
			writeErr(w, http.StatusConflict, "这封定时邮件已经发出，无法"+verb)
			return
		}
		if o.Sealed {
			writeErr(w, http.StatusConflict, "这封定时邮件已经在发送，无法"+verb)
			return
		}
		if !purge {
			if _, err := s.st.SetSendReturnToDraft(opID); err != nil {
				writeErr(w, http.StatusInternalServerError, "取消失败，请稍后重试")
				return
			}
		}
		res, err := s.st.UndoOp(opID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "取消失败，请稍后重试")
			return
		}
		switch res.Mode {
		case "deferred":
			s.broadcastScheduled()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID, "deferred": true})
		case "cancelled":
			if purge {
				_ = os.RemoveAll(outgoingDir(s.st.DataDir(), opID))
				s.broadcastScheduled()
				s.broadcastOps()
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID})
				return
			}
			draftID, merr := s.materializeScheduledDraft(key, o, &sp)
			if merr != nil {
				writeUserErr(w, http.StatusBadGateway, "保存草稿失败", merr)
				return
			}
			_ = os.RemoveAll(outgoingDir(s.st.DataDir(), opID))
			s.broadcastScheduled()
			s.broadcastOps()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "draft_id": draftID})
		case "already_sent":
			writeErr(w, http.StatusConflict, "这封定时邮件已经发出，无法"+verb)
		default:
			writeErr(w, http.StatusConflict, "这封定时邮件已经在发送，无法"+verb)
		}
		return
	}
	if r.URL.Query().Get("purge") == "1" {
		if err := s.st.CancelOp(opID); err != nil {
			writeErr(w, http.StatusConflict, "这封定时邮件已经不能删除了")
			return
		}
		_ = os.RemoveAll(outgoingDir(s.st.DataDir(), opID))
		s.broadcastScheduled()
		s.broadcastOps()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	req := sp.Req
	req.DraftID = 0
	req.Attachments = nil
	dir := outgoingDir(s.st.DataDir(), opID)
	for _, a := range sp.Attachments {
		data, err := os.ReadFile(filepath.Join(dir, a.File))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "定时邮件的附件文件缺失")
			return
		}
		req.Attachments = append(req.Attachments, attachmentIn{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			DataB64:     base64.StdEncoding.EncodeToString(data),
		})
	}
	out, err := s.prepareOutgoing(key, req, false)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "保存草稿失败", err)
		return
	}
	draftID, err := s.appendCopy(key, o.AccountID, "drafts",
		[]imap.Flag{imap.FlagDraft}, mail.FlagSeen|mail.FlagDraft, out)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "保存草稿失败", err)
		return
	}
	if err := s.st.CancelOp(opID); err != nil {
		_ = s.deleteMessageEverywhere(key, draftID)
		writeErr(w, http.StatusConflict, "这封定时邮件已经不能取消了")
		return
	}
	_ = os.RemoveAll(dir)
	s.broadcastMessages([]int64{draftID})
	if fid, _, ok, _ := s.st.FolderBySpecial(o.AccountID, "drafts"); ok {
		s.broadcastFolder(fid)
	}
	s.broadcastScheduled()
	s.broadcastOps()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "draft_id": draftID})
}

type scheduledDetailAtt struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	File        string `json:"file"`
	Size        int64  `json:"size"`
}

type scheduledDetail struct {
	ID          int64                `json:"id"`
	AccountID   int64                `json:"account_id"`
	SendAt      int64                `json:"send_at"`
	Status      string               `json:"status"`
	To          string               `json:"to"`
	Cc          string               `json:"cc"`
	Bcc         string               `json:"bcc"`
	Subject     string               `json:"subject"`
	Text        string               `json:"text"`
	HTML        string               `json:"html"`
	Attachments []scheduledDetailAtt `json:"attachments"`
}

func (s *Server) handleGetScheduled(w http.ResponseWriter, r *http.Request) {
	opID, ok := s.scheduledOpID(w, r)
	if !ok {
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil || o.Type != store.OpSend {
		writeErr(w, http.StatusNotFound, "不存在")
		return
	}
	var sp sendOpPayload
	if err := json.Unmarshal([]byte(o.Payload), &sp); err != nil {
		writeErr(w, http.StatusInternalServerError, "内容格式不对")
		return
	}
	d := scheduledDetail{
		ID: -o.ID, AccountID: o.AccountID, SendAt: o.NotBefore, Status: "pending",
		To: sp.Req.To, Cc: sp.Req.Cc, Bcc: sp.Req.Bcc, Subject: sp.Req.Subject,
		Text: sp.Req.Text, HTML: sp.Req.HTML, Attachments: []scheduledDetailAtt{},
	}
	if o.Status == store.OpNeedsAttention {
		d.Status = "failed"
	}
	for _, a := range sp.Attachments {
		var size int64
		if fi, err := os.Stat(filepath.Join(outgoingDir(s.st.DataDir(), o.ID), a.File)); err == nil {
			size = fi.Size()
		}
		d.Attachments = append(d.Attachments, scheduledDetailAtt{Filename: a.Filename, ContentType: a.ContentType, File: a.File, Size: size})
	}
	writeJSON(w, http.StatusOK, d)
}

type scheduleUpdateReq struct {
	scheduleReq
	KeepFiles []string `json:"keep_files"`
}

func (s *Server) handleUpdateScheduled(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	opID, ok := s.scheduledOpID(w, r)
	if !ok {
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil || o.Type != store.OpSend {
		writeErr(w, http.StatusNotFound, "不存在")
		return
	}
	if o.Status != store.OpPending {
		writeErr(w, http.StatusConflict, "这封定时邮件已经不能修改了")
		return
	}
	var req scheduleUpdateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if req.SendAt <= 0 {
		writeErr(w, http.StatusBadRequest, "请填写发送时间")
		return
	}
	if _, err := s.prepareOutgoing(key, req.composeReq, true); err != nil {
		writeUserErr(w, http.StatusBadRequest, "发送失败，请稍后重试", err)
		return
	}
	var old sendOpPayload
	_ = json.Unmarshal([]byte(o.Payload), &old)
	keep := map[string]bool{}
	for _, f := range req.KeepFiles {
		keep[f] = true
	}
	dir := outgoingDir(s.st.DataDir(), opID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败")
		return
	}
	sp := sendOpPayload{Req: req.composeReq}
	sp.Req.Attachments = nil
	used := map[string]bool{}
	for _, a := range old.Attachments {
		if keep[a.File] {
			sp.Attachments = append(sp.Attachments, a)
			used[a.File] = true
		} else {
			_ = os.Remove(filepath.Join(dir, a.File))
		}
	}
	n := 0
	for _, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataB64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "附件 "+a.Filename+" 的数据无效")
			return
		}
		var name string
		for {
			name = "att" + strconv.Itoa(n)
			n++
			if !used[name] {
				break
			}
		}
		used[name] = true
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
		sp.Attachments = append(sp.Attachments, sendOpAttachment{Filename: a.Filename, ContentType: a.ContentType, File: name})
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败")
		return
	}
	applied, err := s.st.UpdateOpPayload(opID, string(raw))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败")
		return
	}
	if !applied {
		writeErr(w, http.StatusConflict, "这封定时邮件已经不能修改了")
		return
	}
	if _, err := s.st.DB.Exec("UPDATE ops SET not_before = ? WHERE id = ? AND status = 'pending'", req.SendAt, opID); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新失败")
		return
	}
	s.broadcastScheduled()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSendNowOp(w http.ResponseWriter, r *http.Request) {
	opID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "操作编号不对")
		return
	}
	o, err := s.st.GetOp(opID)
	if err != nil || o.Type != store.OpSend {
		s.writeOpStateErr(w, http.StatusNotFound, "操作不存在", opID)
		return
	}
	if err := s.st.AccelerateSendOp(opID); err != nil {
		s.writeOpStateErr(w, http.StatusConflict, "这封邮件已经不能立即发送了", opID)
		return
	}
	s.broadcastOps()
	s.broadcastScheduled()
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}


func firstRecipient(to string) (string, string) {
	first := strings.TrimSpace(strings.Split(to, ",")[0])
	if i := strings.Index(first, "<"); i >= 0 {
		if j := strings.Index(first[i:], ">"); j >= 0 {
			name := strings.Trim(strings.TrimSpace(first[:i]), "\"")
			return name, strings.TrimSpace(first[i+1 : i+j])
		}
	}
	return "", first
}

func snippetOf(text string) string {
	clean := strings.Join(strings.Fields(text), " ")
	r := []rune(clean)
	if len(r) > 140 {
		return string(r[:140])
	}
	return clean
}

func (s *Server) listScheduledAsMessages(w http.ResponseWriter, q url.Values, limit, offset int) {
	labels := map[int64]string{}
	if lrows, err := s.st.DB.Query("SELECT id, label, email FROM accounts"); err == nil {
		defer lrows.Close()
		for lrows.Next() {
			var id int64
			var label, email string
			if err := lrows.Scan(&id, &label, &email); err == nil {
				if label == "" {
					label = email
				}
				labels[id] = label
			}
		}
	}
	rows, err := s.st.DB.Query(`SELECT id, account_id, payload, not_before, status FROM ops
		WHERE type = 'send' AND status IN ('pending', 'in_flight') ORDER BY not_before ASC`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "列表加载失败")
		return
	}
	defer rows.Close()
	all := []messageOut{}
	for rows.Next() {
		var id, accountID, sendAt int64
		var payload, status string
		if err := rows.Scan(&id, &accountID, &payload, &sendAt, &status); err != nil {
			writeErr(w, http.StatusInternalServerError, "列表加载失败")
			return
		}
		mo := messageOut{
			ID:            -id,
			AccountID:     accountID,
			AccountLabel:  labels[accountID],
			FolderSpecial: "scheduled",
			DateTS:        sendAt,
			Flags:         1,
			OpStatus:      status,
		}
		var sp sendOpPayload
		if err := json.Unmarshal([]byte(payload), &sp); err == nil {
			mo.Subject = sp.Req.Subject
			mo.FromName, mo.FromEmail = firstRecipient(sp.Req.To)
			mo.Snippet = snippetOf(sp.Req.Text)
			mo.HasAttach = len(sp.Attachments) > 0
		}
		all = append(all, mo)
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	page := all[offset:end]
	if wm, err := s.st.ListVersion(); err == nil {
		w.Header().Set("X-WM-Ver", strconv.FormatInt(wm, 10))
	}
	out := []threadOut{}
	for _, mo := range page {
		out = append(out, threadOut{messageOut: mo, ThreadCount: 1})
	}
	writeJSON(w, http.StatusOK, out)
}
