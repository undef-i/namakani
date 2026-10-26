package api

import (
	"errors"
	"testing"

	"namakani/internal/store"
)

func TestDrainAccelerationSkipsOpsFailedEarlierInSameRun(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.addMessage(t, 11, 4, "Drafts", 400, 0)
	for i := 0; i < 10; i++ {
		f.p.failStoreFlags = append(f.p.failStoreFlags, errors.New("dial timeout"))
	}
	if _, err := f.srv.enqueueSendOp(f.srv.keys["tok"], composeReq{
		AccountID: 1, To: "b@x.test", Subject: "hi", Text: "body", DraftID: 11,
	}, 0, ""); err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	rec := f.postFlags(t, 10, `{"seen":true}`)
	if rec.Code != 200 {
		t.Fatalf("flags handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	flagsOpID := opIDOf(t, rec)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	o, err := f.st.GetOp(flagsOpID)
	if err != nil {
		t.Fatalf("get flags op: %v", err)
	}
	if o.Status != store.OpPending {
		t.Fatalf("flags op status = %q, want pending (backed off, not parked)", o.Status)
	}
	if o.Attempts != 1 {
		t.Fatalf("flags op attempts = %d, want 1: a success later in the same drain run (the send, then its draft expunge) must not accelerate an op that already failed in this run into an immediate retry", o.Attempts)
	}
}
