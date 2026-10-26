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

