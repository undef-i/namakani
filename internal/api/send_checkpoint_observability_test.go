package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/store"
)

// D3: the checkpoint must be observable the moment it is crossed.
// Before the fix the seal published nothing at all: the send bar
// kept offering a live undo for a send whose fate was already
// decided, and an undo clicked in that window was answered
// "deferred" with no way to tell it from a pre-checkpoint undo whose
// verdict was still open.
func TestSealPublishesSealedEventAndUndoAnswerDistinguishes(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	events := f.collectOpEvents(t)
	deliverEntered := make(chan struct{})
	deliverRelease := make(chan struct{})
	var enteredOnce sync.Once
	var deliverCalls int32
	f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
		atomic.AddInt32(&deliverCalls, 1)
		enteredOnce.Do(func() { close(deliverEntered) })
		<-deliverRelease
		return "provider-sent-1", nil
	}
	opID := f.sendOp(t)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.srv.drainAccount(f.srv.keys["tok"], 1)
	}()
	select {
	case <-deliverEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never reached the provider call")
	}
	// The op is inside the provider call, so the checkpoint is
	// crossed: a sealed event for this op must already be published,
	// before delivery completes.
	sealedPublished := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Status == "sealed" {
			sealedPublished = true
		}
	}
	if !sealedPublished {
		t.Fatal("crossing the checkpoint published no sealed event; clients keep offering an undo the server can no longer honor on request")
	}
	// An undo clicked now is recorded, but its answer must carry the
	// sealed fact so the interface can retire the button instead of
	// presenting the click as an open verdict.
	rec := f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo after the checkpoint status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode   string `json:"mode"`
		Sealed bool   `json:"sealed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("undo body: %v", err)
	}
	if !resp.Sealed {
		t.Fatalf("undo after the checkpoint answer = %s; it does not carry the sealed fact, so the interface cannot distinguish it from a pre-checkpoint deferred undo", rec.Body.String())
	}
	close(deliverRelease)
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("drain did not finish after delivery was released")
	}
	if got := atomic.LoadInt32(&deliverCalls); got != 1 {
		t.Fatalf("deliver calls = %d, want 1", got)
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done", o.Status)
	}
	// The terminal publication still lands as before: done + sent +
	// undo_ignored stays available as the reconciliation signal.
	told := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Sent && ev.UndoIgnored {
			told = true
		}
	}
	if !told {
		t.Fatal("the terminal done event lost its undo_ignored reconciliation flag")
	}
}

// The pre-checkpoint answers must stay exactly what they were: an
// undo against a still-pending send cancels it, and its answer
// carries no sealed claim.
