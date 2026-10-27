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
	sealedPublished := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Status == "sealed" {
			sealedPublished = true
		}
	}
	if !sealedPublished {
		t.Fatal("crossing the checkpoint published no sealed event; clients keep offering an undo the server can no longer honor on request")
	}
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
