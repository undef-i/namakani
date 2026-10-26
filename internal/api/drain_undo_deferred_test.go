package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type slowMoveConn struct {
	mail.Conn
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func (c *slowMoveConn) MoveMessage(uid uint32, dest string) (uint32, error) {
	c.once.Do(func() { close(c.started) })
	time.Sleep(c.delay)
	return c.Conn.MoveMessage(uid, dest)
}

func TestUndoMoveWhileInFlightDefersThenReverses(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	slow := &slowMoveConn{Conn: f.p.conn(), delay: 800 * time.Millisecond, started: make(chan struct{})}
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		return slow, nil
	}
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.srv.drainAccount(nil, 1)
	}()
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never reached the provider move")
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpInFlight {
		t.Fatalf("op status while the provider is working = %q, want in_flight", o.Status)
	}
	rec = f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo during flight status = %d, body %s; the user must not be told to try again later", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Mode != "deferred" {
		t.Fatalf("undo during flight mode = %q, want deferred (body %s)", resp.Mode, rec.Body.String())
	}
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("drain did not finish")
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("original op status = %q, want done", o.Status)
	}
	route, _ := f.st.MessageRoute(10)
	if route.FolderID != 1 {
		t.Fatalf("mirror folder after the deferred undo drained = %d, want 1 (INBOX)", route.FolderID)
	}
	if _, ok := f.p.flags("INBOX", route.UID); !ok {
		t.Fatal("provider does not hold the message in INBOX after the deferred undo drained")
	}
	calls := f.p.callsOf("MoveMessage")
	if len(calls) != 2 {
		t.Fatalf("provider MoveMessage calls = %d, want 2 (move out, deferred-undo move back)", len(calls))
	}
	if calls[1].Folder != "Archive" || calls[1].Dest != "INBOX" {
		t.Fatalf("inverse move addressed %+v, want source Archive dest INBOX", calls[1])
	}
}

func TestExecuteSendHonorsUndoRegisteredInFlight(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	req := f.authedReq(http.MethodPost, "/api/send", `{"account_id":1,"to":"b@x.test","subject":"hi","text":"body","undo_seconds":0}`)
	rec := httptest.NewRecorder()
	f.srv.handleSend(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OpID int64 `json:"op_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.OpID == 0 {
		t.Fatalf("send response = %s, want an op_id", rec.Body.String())
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", resp.OpID); err != nil {
		t.Fatalf("force in_flight: %v", err)
	}
	undoRes, err := f.st.UndoOp(resp.OpID)
	if err != nil {
		t.Fatalf("undo of an in-flight send must register intent, not fail: %v", err)
	}
	if undoRes.Mode != "deferred" {
		t.Fatalf("undo mode = %q, want deferred", undoRes.Mode)
	}
	o, err := f.st.GetOp(resp.OpID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if !f.srv.executeSend(f.srv.keys["tok"], o) {
		t.Fatal("executeSend must report a clean stop for an undo-registered send")
	}
	if seams.deliverCalls != 0 {
		t.Fatalf("deliver calls = %d, want 0 (undo was registered before delivery)", seams.deliverCalls)
	}
	o, _ = f.st.GetOp(resp.OpID)
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
	if _, err := os.Stat(filepath.Join(f.srv.st.DataDir(), "outgoing", fmt.Sprint(resp.OpID))); !os.IsNotExist(err) {
		t.Fatal("outgoing payload dir survived an in-flight undo")
	}
}
