package api

import (
	"testing"

	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

func TestParkedMoveBroadcastsOpState(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.failMove = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	events := f.collectOpEvents(t)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("setup: op status = %q, want needs_attention", o.Status)
	}
	found := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Status == store.OpNeedsAttention {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ops event with status needs_attention for parked op %d", opID)
	}
}
