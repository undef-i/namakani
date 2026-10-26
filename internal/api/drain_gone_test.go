package api

import (
	"testing"

	"namakani/internal/store"
)

func TestDrainExpungeWhoseMessageAlreadyVanishedCompletes(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 3, "Trash", 100, 0)
	opID, err := f.st.EnqueueExpunge(1, 10, "gone-expunge")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if err := f.st.DeleteRowOnly(10); err != nil {
		t.Fatalf("remove local row: %v", err)
	}
	delete(f.p.folders["Trash"], 100)
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q (last_error %q), want done: the message is already gone everywhere, so the expunge has nothing left to do", o.Status, o.LastError)
	}
	if o.LastError != "" {
		t.Fatalf("op last_error = %q, want empty for a completed expunge", o.LastError)
	}
}

func TestDrainFlagsWhoseMessageAlreadyVanishedCompletes(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	if err := f.st.DeleteRowOnly(10); err != nil {
		t.Fatalf("remove local row: %v", err)
	}
	delete(f.p.folders["INBOX"], 100)
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q (last_error %q), want done: the message is already gone everywhere, so the flag change has nothing left to apply to", o.Status, o.LastError)
	}
	if o.LastError != "" {
		t.Fatalf("op last_error = %q, want empty for a completed flag change", o.LastError)
	}
}
