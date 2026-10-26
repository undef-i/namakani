package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"namakani/internal/auth"
	"namakani/internal/store"
)

type sendSeams struct {
	deliverCalls int
	fileCalls    int
	deliverErr   error
	fileErrs     []error
	sentIDs      []string
}

func (f *drainFixture) armSend(t *testing.T) *sendSeams {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	enc, err := auth.Encrypt(key, "pw")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE accounts SET enc_password = ? WHERE id = 1", enc); err != nil {
		t.Fatalf("set account password: %v", err)
	}
	token, err := auth.NewSession(f.st, "")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	f.sessionToken = token
	f.srv.keys[token] = key
	f.srv.keys["tok"] = key
	seams := &sendSeams{}
	f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
		seams.deliverCalls++
		if seams.deliverErr != nil {
			return "", seams.deliverErr
		}
		seams.sentIDs = append(seams.sentIDs, out.messageID)
		return "provider-sent-1", nil
	}
	f.srv.sendFileCopy = func(k []byte, out *outgoing, sentID string) (int64, error) {
		seams.fileCalls++
		if len(seams.fileErrs) > 0 {
			err := seams.fileErrs[0]
			seams.fileErrs = seams.fileErrs[1:]
			if err != nil {
				return 0, err
			}
		}
		return 777, nil
	}
	return seams
}

func (f *drainFixture) authedReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "wm_session", Value: f.sessionToken})
	return req
}

func (f *drainFixture) forceDue(t *testing.T, opID int64) {
	t.Helper()
	if _, err := f.st.DB.Exec("UPDATE ops SET not_before = 0, next_attempt = 0 WHERE id = ?", opID); err != nil {
		t.Fatalf("force due: %v", err)
	}
}

func TestSendHandlerEnqueuesWithUndoWindow(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	before := time.Now().Unix()
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":30}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OpID      int64 `json:"op_id"`
		NotBefore int64 `json:"not_before"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.OpID == 0 {
		t.Fatalf("send response = %s, want an op_id", rec.Body.String())
	}
	if resp.NotBefore < before+25 || resp.NotBefore > before+35 {
		t.Fatalf("not_before = %d, want about now+30 (the undo window)", resp.NotBefore)
	}
	if seams.deliverCalls != 0 {
		t.Fatal("delivery happened inside the request; sending must wait for the drainer")
	}
	f.srv.drainAccount(nil, 1)
	if seams.deliverCalls != 0 {
		t.Fatal("delivery happened before the undo window closed")
	}
	f.forceDue(t, resp.OpID)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if seams.deliverCalls != 1 {
		t.Fatalf("deliver calls after the window = %d, want 1", seams.deliverCalls)
	}
	o, _ := f.st.GetOp(resp.OpID)
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done", o.Status)
	}
	if _, err := os.Stat(filepath.Join(f.srv.st.DataDir(), "outgoing", fmt.Sprint(resp.OpID))); !os.IsNotExist(err) {
		t.Fatal("outgoing payload dir survived a completed send")
	}
}

func TestSendUndoInsideWindowCancelsDelivery(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":30}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	opID := opIDOf(t, rec)
	rec = f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body %s", rec.Code, rec.Body.String())
	}
	f.forceDue(t, opID)
	f.srv.drainAccount(nil, 1)
	if seams.deliverCalls != 0 {
		t.Fatal("a send cancelled inside its undo window was delivered anyway")
	}
	if _, err := os.Stat(filepath.Join(f.srv.st.DataDir(), "outgoing", fmt.Sprint(opID))); !os.IsNotExist(err) {
		t.Fatal("outgoing payload dir survived a cancelled send")
	}
}

func TestSendArchiveFailureRetriesFilingWithoutRedelivery(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	seams.fileErrs = []error{errors.New("append boom"), errors.New("append boom")}
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":0}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	opID := opIDOf(t, rec)
	for round := 0; round < 3; round++ {
		f.forceDue(t, opID)
		f.srv.drainAccount(f.srv.keys["tok"], 1)
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after filing finally succeeds = %q, want done", o.Status)
	}
	if seams.deliverCalls != 1 {
		t.Fatalf("deliver calls = %d, want exactly 1 (a filing retry must never resend the mail)", seams.deliverCalls)
	}
	if seams.fileCalls != 3 {
		t.Fatalf("file calls = %d, want 3 (two failures then success)", seams.fileCalls)
	}
}

func TestSendPermanentFailureParksImmediately(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	seams.deliverErr = &textproto.Error{Code: 550, Msg: "mailbox unavailable"}
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":0}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("op status after an SMTP 550 = %q, want needs_attention without burning retries", o.Status)
	}
	if seams.deliverCalls != 1 {
		t.Fatalf("deliver calls = %d, want 1", seams.deliverCalls)
	}
}


func TestScheduledEndpointsRunOnOps(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	req := f.authedReq(http.MethodPost, "/api/send/schedule", `{"account_id":1,"to":"b@x.test","subject":"later","text":"body","send_at":1899999999}`)
	rec := httptest.NewRecorder()
	f.srv.handleScheduleSend(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("schedule status = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID >= 0 {
		t.Fatalf("scheduled id = %d, want a negative pseudo id backed by an op", created.ID)
	}
	rec = httptest.NewRecorder()
	f.srv.handleListScheduled(rec, httptest.NewRequest(http.MethodGet, "/api/scheduled", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"subject":"later"`) {
		t.Fatalf("scheduled list = %d %s, want the op listed with its subject", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/scheduled/%d", created.ID), nil)
	req.SetPathValue("id", fmt.Sprint(created.ID))
	rec = httptest.NewRecorder()
	f.srv.handleCancelScheduled(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel scheduled status = %d, body %s", rec.Code, rec.Body.String())
	}
	var active int
	_ = f.st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE type = 'send' AND status IN ('pending','in_flight','needs_attention')").Scan(&active)
	if active != 0 {
		t.Fatalf("active send ops after cancel = %d, want 0", active)
	}
}

func TestSendDraftCleanupFollowsRebuiltDraft(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 30, 1, "INBOX", 300, 0)
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":30,"draft_id":30}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)
	// The drafts folder is rebuilt before the send executes: the draft row
	// the request named is gone, the draft itself returns under a new row.
	if err := f.st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(88, 1, 1, 340, 'm30@x', 0)`); err != nil {
		t.Fatalf("reinsert draft: %v", err)
	}
	f.p.seed("INBOX", 340, 0)
	f.forceDue(t, opID)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("send op status = %q, want done", o.Status)
	}
	var expungeIdentity string
	var expungeRow int64
	if err := f.st.DB.QueryRow("SELECT identity, message_id FROM ops WHERE type = 'expunge' ORDER BY id DESC LIMIT 1").
		Scan(&expungeIdentity, &expungeRow); err != nil {
		t.Fatalf("find draft cleanup expunge: %v", err)
	}
	if expungeRow != 88 || expungeIdentity != "m:m30@x" {
		t.Fatalf("cleanup expunge binds row %d identity %q, want row 88 identity m:m30@x (the rebuilt draft)", expungeRow, expungeIdentity)
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	found := false
	for _, c := range f.p.callsOf("DeleteMessage") {
		for _, u := range c.UIDs {
			if u == 340 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("DeleteMessage calls = %+v, want the rebuilt draft uid 340 deleted", f.p.callsOf("DeleteMessage"))
	}
}
