package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

type attentionItem struct {
	OpID      int64  `json:"op_id"`
	Type      string `json:"type"`
	MessageID int64  `json:"message_id"`
	Subject   string `json:"subject"`
	Error     string `json:"error"`
}

func listAttention(t *testing.T, f *drainFixture) []attentionItem {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleListAttention(rec, httptest.NewRequest(http.MethodGet, "/api/ops/attention", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("attention list status = %d, body %s", rec.Code, rec.Body.String())
	}
	var items []attentionItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode attention list: %v (body %s)", err, rec.Body.String())
	}
	return items
}

func TestAttentionListShowsParkedOpWithError(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	if _, err := f.st.DB.Exec("UPDATE messages SET subject = 'quarterly report' WHERE id = 10"); err != nil {
		t.Fatal(err)
	}
	f.p.failStoreFlags = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("setup: op status = %q, want needs_attention", o.Status)
	}
	items := listAttention(t, f)
	if len(items) != 1 {
		t.Fatalf("attention items = %d, want 1", len(items))
	}
	it := items[0]
	if it.OpID != opID || it.Type != store.OpFlags || it.MessageID != 10 {
		t.Fatalf("attention item = %+v, want the parked flags op for message 10", it)
	}
	if it.Subject != "quarterly report" {
		t.Fatalf("attention subject = %q, want the message subject for context", it.Subject)
	}
	if it.Error == "" {
		t.Fatal("attention item carries no error text; the user cannot tell what went wrong")
	}
}

func TestAttentionRetryRequeuesParkedOp(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.failStoreFlags = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/ops/"+strconv.FormatInt(opID, 10)+"/retry", nil)
	req2.SetPathValue("id", strconv.FormatInt(opID, 10))
	f.srv.handleRetryOp(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body %s", rec2.Code, rec2.Body.String())
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpPending {
		t.Fatalf("op status after retry = %q, want pending", o.Status)
	}
	if items := listAttention(t, f); len(items) != 0 {
		t.Fatalf("attention items after retry = %d, want 0", len(items))
	}
	f.srv.drainAccount(nil, 1)
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after retry + drain = %q, want done", o.Status)
	}
}

func TestAttentionDiscardParkedMoveRestoresMirror(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.failMove = []error{
		errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom"),
		errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom"),
	}
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	for i := 0; i < 8; i++ {
		f.forceDue(t, opID)
		f.srv.drainAccount(nil, 1)
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("setup: op status = %q, want needs_attention after 8 failures", o.Status)
	}
	route, _ := f.st.MessageRoute(10)
	if route.FolderID != 2 {
		t.Fatalf("setup: mirror folder = %d, want 2 (the local move stands while parked)", route.FolderID)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/ops/"+strconv.FormatInt(opID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(opID, 10))
	rec = httptest.NewRecorder()
	f.srv.handleDiscardOp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discard status = %d, body %s", rec.Code, rec.Body.String())
	}
	route, _ = f.st.MessageRoute(10)
	if route.FolderID != 1 || route.UID != 100 {
		t.Fatalf("route after discarding a parked move = folder %d uid %d, want the exact prior location 1/100", route.FolderID, route.UID)
	}
}

func TestAttentionSubjectFollowsRebuiltIncarnation(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	if _, err := f.st.DB.Exec("UPDATE messages SET subject = 'quarterly report' WHERE id = 10"); err != nil {
		t.Fatal(err)
	}
	f.p.failStoreFlags = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("setup: op status = %q, want needs_attention", o.Status)
	}
	// The folder is rebuilt underneath the parked op: the old row is gone
	// and the same message returns under a new row id and uid.
	if err := f.st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject, flags)
		VALUES(77, 1, 1, 140, 'm10@x', 'quarterly report', 0)`); err != nil {
		t.Fatalf("reinsert: %v", err)
	}
	items := listAttention(t, f)
	if len(items) != 1 {
		t.Fatalf("attention items = %d, want 1", len(items))
	}
	it := items[0]
	if it.MessageID != 77 {
		t.Fatalf("attention message_id = %d, want 77 (the current incarnation)", it.MessageID)
	}
	if it.Subject != "quarterly report" {
		t.Fatalf("attention subject = %q, want the rebuilt row's subject; a dead hint row must not blank it", it.Subject)
	}
}
