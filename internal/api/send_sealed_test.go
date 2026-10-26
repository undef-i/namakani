package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/store"
)

// D2, adversarial: a send executor seals its op and then hangs inside
// the provider call past the watchdog budget. The wave is retired and
// the reaper runs. The sealed op must not return to pending, must not
// be delivered a second time when the hung call finally returns, and
// its books must end at "outcome unknown" — never at a cancelled or
// retried state that asserts a delivery fact nobody knows.
func TestSealedSendSurvivesWatchdogRetireWithoutRedelivery(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	f.srv.claimStuckSecs = 1
	deliverEntered := make(chan struct{})
	deliverRelease := make(chan struct{})
	var deliverCalls int32
	var enteredOnce sync.Once
	f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
		atomic.AddInt32(&deliverCalls, 1)
		enteredOnce.Do(func() { close(deliverEntered) })
		<-deliverRelease
		return "provider-sent-late", nil
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
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpInFlight || !o.Sealed {
		t.Fatalf("op inside the provider call = (status %q, sealed %v), want (in_flight, true)", o.Status, o.Sealed)
	}
	// The watchdog retires the stuck wave and the reaper runs.
	f.srv.killStuckWaves(time.Now().Unix() + f.srv.claimStuckSecs + 1)
	f.srv.reclaimOrphanedClaims()
	o, _ = f.st.GetOp(opID)
	if o.Status == store.OpPending {
		t.Fatal("the reaper returned a sealed send to pending; a new generation will seal and deliver it a second time")
	}
	if o.Status != store.OpNeedsAttention || o.LastError != store.SendOutcomeUnknownError {
		t.Fatalf("sealed orphan after reclaim = (status %q, error %q), want needs_attention with the outcome-unknown marker", o.Status, o.LastError)
	}
	// The hung provider call finally succeeds. The zombie executor
	// must not land anything: no completion, no filed copy, and above
	// all no second delivery.
	close(deliverRelease)
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("zombie drain did not exit")
	}
	if got := atomic.LoadInt32(&deliverCalls); got != 1 {
		t.Fatalf("provider deliveries = %d, want exactly 1", got)
	}
	if seams.fileCalls != 0 {
		t.Fatalf("filed copies = %d, want 0 (the fenced executor may not land its late success over the parked record)", seams.fileCalls)
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention || o.LastError != store.SendOutcomeUnknownError {
		t.Fatalf("op after the zombie returned = (status %q, error %q), want the parked outcome-unknown record untouched", o.Status, o.LastError)
	}
	// An undo now must get the terminal sealed answer, and the books
	// must not move: claiming this letter was cancelled would assert
	// it was never sent, which nobody knows.
	rec := f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo of a parked send status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Mode != "sealed" {
		t.Fatalf("undo of a parked send mode = %q (body %s), want sealed", resp.Mode, rec.Body.String())
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("op status after the sealed undo answer = %q, want needs_attention", o.Status)
	}
	// A retry must be refused, and a fresh drain must deliver nothing.
	req := f.authedReq(http.MethodPost, "/api/ops/"+strconv.FormatInt(opID, 10)+"/retry", "")
	req.SetPathValue("id", strconv.FormatInt(opID, 10))
	rec = httptest.NewRecorder()
	f.srv.handleRetryOp(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("retry of a parked send status = %d, body %s; want 409", rec.Code, rec.Body.String())
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if got := atomic.LoadInt32(&deliverCalls); got != 1 {
		t.Fatalf("provider deliveries after a fresh drain = %d, want exactly 1", got)
	}
}

// The other sealed orphan shape: delivery succeeded and the Sent
// marker is on the payload, but the executor died before filing the
// sent copy and completing. The settler finishes exactly those books
// and never calls the provider again.
func TestSealedSentSendIsCompletedBySettlerWithoutRedelivery(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	f.srv.claimStuckSecs = 1
	fileEntered := make(chan struct{})
	fileRelease := make(chan struct{})
	var fileCalls int32
	var enteredOnce sync.Once
	f.srv.sendFileCopy = func(k []byte, out *outgoing, sentID string) (int64, error) {
		if atomic.AddInt32(&fileCalls, 1) == 1 {
			enteredOnce.Do(func() { close(fileEntered) })
			<-fileRelease
		}
		return 777, nil
	}
	opID := f.sendOp(t)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.srv.drainAccount(f.srv.keys["tok"], 1)
	}()
	select {
	case <-fileEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never reached filing the sent copy")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpInFlight || !o.Sealed {
		t.Fatalf("op during filing = (status %q, sealed %v), want (in_flight, true)", o.Status, o.Sealed)
	}
	f.srv.killStuckWaves(time.Now().Unix() + f.srv.claimStuckSecs + 1)
	f.srv.reclaimOrphanedClaims()
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("sealed sent op after reclaim = %q, want done (the settler completes the books of a proven delivery)", o.Status)
	}
	if seams.deliverCalls != 1 {
		t.Fatalf("provider deliveries = %d, want exactly 1", seams.deliverCalls)
	}
	close(fileRelease)
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("zombie drain did not exit")
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after the zombie returned = %q, want done", o.Status)
	}
	if seams.deliverCalls != 1 {
		t.Fatalf("provider deliveries after the zombie returned = %d, want exactly 1", seams.deliverCalls)
	}
}
