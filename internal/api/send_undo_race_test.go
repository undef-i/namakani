package api

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/store"
)

// The undo invariant, adversarially: undo requests land at random
// offsets around the end of the undo window while a slow provider
// stretches the execution phase. For every round, two statements must
// hold together:
//   - an undo the server accepted while the op was still pending
//     (mode "cancelled") means the letter was never delivered;
//   - whenever the letter was delivered, the server had already
//     published the op's departure from pending (an in_flight op
//     event): the send bar's lifetime is server state, so a click can
//     never be offered for a send whose fate is already sealed.

func undoMode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body %s; an undo click must always get a state-machine answer, never a server error", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("undo body: %v", err)
	}
	return resp.Mode
}

func TestUndoWhilePendingAlwaysCancelsAndNeverDelivers(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":300}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)
	if mode := undoMode(t, f.postUndo(t, opID)); mode != "cancelled" {
		t.Fatalf("undo while pending mode = %q, want cancelled", mode)
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if seams.deliverCalls != 0 {
		t.Fatal("a send whose undo was accepted while pending was delivered anyway")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
}

func TestSendUndoRaceRounds(t *testing.T) {
	rng := rand.New(rand.NewSource(20261026))
	for round := 0; round < 40; round++ {
		f := newDrainFixture(t)
		f.armSend(t)
		events := f.collectRawEvents(t)
		var deliverCalls int32
		f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
			atomic.AddInt32(&deliverCalls, 1)
			time.Sleep(time.Duration(rng.Intn(3000)) * time.Microsecond)
			return "provider-sent-race", nil
		}
		req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":0}`)
		rec := httptest.NewRecorder()
		f.srv.handleSend(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("round %d: send status = %d, body %s", round, rec.Code, rec.Body.String())
		}
		opID := opIDOf(t, rec)
		go f.srv.drainAccount(f.srv.keys["tok"], 1)
		time.Sleep(time.Duration(rng.Intn(3000)) * time.Microsecond)
		mode := undoMode(t, f.postUndo(t, opID))
		// The send may be executed by this drain call or by the one the
		// send handler itself kicked; wait for the op's terminal state
		// either way.
		var o *store.Op
		deadline := time.Now().Add(15 * time.Second)
		for {
			var err error
			o, err = f.st.GetOp(opID)
			if err != nil {
				t.Fatalf("round %d: get op: %v", round, err)
			}
			if o.Status == store.OpDone || o.Status == store.OpCancelled {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: op never landed, status = %q", round, o.Status)
			}
			time.Sleep(2 * time.Millisecond)
		}
		delivered := atomic.LoadInt32(&deliverCalls)
		if mode == "cancelled" {
			if delivered != 0 {
				t.Fatalf("round %d: undo was accepted as cancelled but the letter was delivered", round)
			}
			if o.Status != store.OpCancelled {
				t.Fatalf("round %d: op status = %q after a cancelled undo, want cancelled", round, o.Status)
			}
			continue
		}
		if delivered == 1 && o.Status != store.OpDone {
			t.Fatalf("round %d: delivered once but op status = %q, want done", round, o.Status)
		}
		if delivered == 0 && o.Status != store.OpCancelled {
			t.Fatalf("round %d: never delivered but op status = %q, want cancelled", round, o.Status)
		}
		if mode == "deferred" {
			published := false
			for _, ev := range events() {
				if ev.Type == "ops" && ev.OpID == opID && ev.Status == store.OpInFlight {
					published = true
				}
			}
			if !published {
				t.Fatalf("round %d: undo was deferred against an in-flight send, but the server never published the op leaving pending; a client bar driven by server state could still have offered that click", round)
			}
		}
	}
}
