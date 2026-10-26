package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/store"
)

type opStateEvent struct {
	Type        string `json:"type"`
	OpID        int64  `json:"op_id"`
	OpType      string `json:"op_type"`
	Status      string `json:"status"`
	Sent        bool   `json:"sent"`
	UndoIgnored bool   `json:"undo_ignored"`
}

func (f *drainFixture) collectOpEvents(t *testing.T) func() []opStateEvent {
	t.Helper()
	ch, unsub := f.srv.hub.subscribe()
	t.Cleanup(unsub)
	return func() []opStateEvent {
		var out []opStateEvent
		for {
			select {
			case data := <-ch:
				var ev opStateEvent
				if err := json.Unmarshal(data, &ev); err == nil && ev.Type == "ops" && ev.OpID != 0 {
					out = append(out, ev)
				}
			default:
				return out
			}
		}
	}
}

func (f *drainFixture) sendOp(t *testing.T) int64 {
	t.Helper()
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":0}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	return opIDOf(t, rec)
}

func TestUndoDeliveredSendReportsAlreadySent(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	events := f.collectOpEvents(t)
	opID := f.sendOp(t)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if seams.deliverCalls != 1 || seams.fileCalls != 1 {
		t.Fatalf("after drain: deliver = %d, file = %d; want exactly one delivery and one filed copy", seams.deliverCalls, seams.fileCalls)
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done", o.Status)
	}
	announced := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Sent && ev.Status == store.OpDone {
			announced = true
		}
	}
	if !announced {
		t.Fatal("completion of a delivered send was never broadcast as an op state event; clients keep showing an undo button for a mail that is already gone")
	}
	rec := f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo of a delivered send status = %d, body %s; it must be a 200 already_sent answer, not an error", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Mode != "already_sent" {
		t.Fatalf("undo of a delivered send mode = %q, want already_sent (body %s)", resp.Mode, rec.Body.String())
	}
	res, err := f.st.UndoOp(opID)
	if err != nil {
		t.Fatalf("store undo of a delivered send: %v", err)
	}
	if res.Mode != "already_sent" || res.Inverse != nil {
		t.Fatalf("store undo of a delivered send = %+v, want already_sent with no inverse op", res)
	}
	var undone, total int
	_ = f.st.DB.QueryRow("SELECT undone FROM ops WHERE id = ?", opID).Scan(&undone)
	_ = f.st.DB.QueryRow("SELECT COUNT(*) FROM ops").Scan(&total)
	if undone != 0 {
		t.Fatal("the send op was marked undone even though nothing was undone")
	}
	if total != 1 {
		t.Fatalf("ops after undoing a delivered send = %d, want 1 (no inverse op may be created)", total)
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after the already_sent answer = %q, want done (the filed copy must stay)", o.Status)
	}
}

func TestSendUndoIntentDuringDeliveryEndsAlreadySent(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return "provider-sent-9", nil
	}
	events := f.collectOpEvents(t)
	opID := f.sendOp(t)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.srv.drainAccount(f.srv.keys["tok"], 1)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never reached delivery")
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpInFlight {
		t.Fatalf("op status during delivery = %q, want in_flight", o.Status)
	}
	rec := f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo during delivery status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Mode != "deferred" {
		t.Fatalf("undo during delivery mode = %q, want deferred (body %s)", resp.Mode, rec.Body.String())
	}
	close(release)
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("drain did not finish after delivery was released")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("deliver calls = %d, want exactly 1", got)
	}
	if seams.fileCalls != 1 {
		t.Fatalf("filed copies = %d, want 1 (the sent copy must be filed and kept)", seams.fileCalls)
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done (delivery succeeded; the op completes normally)", o.Status)
	}
	if o.UndoRequested {
		t.Fatal("the undo intent is still on the op; it must be consumed once delivery has happened, never turned into a copy-deleting reversal")
	}
	var total int
	_ = f.st.DB.QueryRow("SELECT COUNT(*) FROM ops").Scan(&total)
	if total != 1 {
		t.Fatalf("ops = %d, want 1 (no inverse op may pretend the send was undone)", total)
	}
	told := false
	for _, ev := range events() {
		if ev.OpID == opID && ev.Sent && ev.UndoIgnored {
			told = true
		}
	}
	if !told {
		t.Fatal("clients were never told the send completed with the undo unhonored; the sender keeps a live undo button for a delivered mail")
	}
}
