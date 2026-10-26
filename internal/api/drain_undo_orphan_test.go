package api

import (
	"testing"
	"time"

	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

func TestParkedOpForgetsUndoRequest(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	ops, err := f.st.ClaimDueOps(1, time.Now().Unix(), 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("claim = %v ops, err %v; want the flags op in flight", len(ops), err)
	}
	res, err := f.st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo while in flight: %v", err)
	}
	if res.Mode != "deferred" {
		t.Fatalf("undo mode = %q, want deferred", res.Mode)
	}
	f.p.failStoreFlags = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("op status = %q, want needs_attention", o.Status)
	}
	if o.UndoRequested {
		t.Fatal("the parked op still carries undo_requested; the intent registered against its failed execution must be void, or a future retry will execute a forgotten undo")
	}
}
