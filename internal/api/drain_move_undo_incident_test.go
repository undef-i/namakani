package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type noDestUIDConn struct {
	mail.Conn
}

func (c *noDestUIDConn) MoveMessage(uid uint32, dest string) (uint32, error) {
	_, err := c.Conn.MoveMessage(uid, dest)
	return 0, err
}

func (c *noDestUIDConn) FetchUIDs() ([]uint32, error) {
	if l, ok := c.Conn.(mail.UIDLister); ok {
		return l.FetchUIDs()
	}
	return nil, errNotUIDLister
}

var errNotUIDLister = errors.New("wrapped conn cannot list uids")

type slowNoUIDConn struct {
	mail.Conn
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func (c *slowNoUIDConn) MoveMessage(uid uint32, dest string) (uint32, error) {
	c.once.Do(func() { close(c.started) })
	time.Sleep(c.delay)
	_, err := c.Conn.MoveMessage(uid, dest)
	return 0, err
}

func (c *slowNoUIDConn) FetchUIDs() ([]uint32, error) {
	if l, ok := c.Conn.(mail.UIDLister); ok {
		return l.FetchUIDs()
	}
	return nil, errNotUIDLister
}

func providerCount(p *fakeProvider, folder string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.folders[folder])
}

func (f *drainFixture) addTrashMessageNoID(t *testing.T, id int64, uid uint32) {
	t.Helper()
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(?, 1, 3, ?, '', 0)`, id, uid); err != nil {
		t.Fatalf("insert trash message %d: %v", id, err)
	}
	f.p.seed("Trash", uid, 0)
}

func opUndone(t *testing.T, f *drainFixture, id int64) int {
	t.Helper()
	var u int
	if err := f.st.DB.QueryRow("SELECT undone FROM ops WHERE id = ?", id).Scan(&u); err != nil {
		t.Fatalf("read undone for op %d: %v", id, err)
	}
	return u
}

func TestMoveLandedWithoutDestUIDKeepsMirrorRow(t *testing.T) {
	f := newDrainFixture(t)
	f.addTrashMessageNoID(t, 10, 181)
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		return &noDestUIDConn{f.p.conn()}, nil
	}
	rec := f.postMove(t, 10, `{"to":"inbox"}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done (the provider did move the message)", o.Status)
	}
	route, err := f.st.MessageRoute(10)
	if err != nil {
		t.Fatal("mirror row was dropped for a message the provider moved successfully; the message is lost locally")
	}
	if route.FolderID != 1 {
		t.Fatalf("mirror folder = %d, want 1 (INBOX)", route.FolderID)
	}
	if n := providerCount(f.p, "INBOX"); n != 1 {
		t.Fatalf("provider INBOX holds %d copies, want exactly 1", n)
	}
	if n := providerCount(f.p, "Trash"); n != 0 {
		t.Fatalf("provider Trash holds %d copies, want 0", n)
	}
	if _, ok := f.p.flags("INBOX", route.UID); !ok {
		t.Fatal("mirror uid does not resolve to the provider copy in INBOX")
	}
}

func TestDeferredUndoOfRestoreLandedWithoutDestUID(t *testing.T) {
	f := newDrainFixture(t)
	f.addTrashMessageNoID(t, 10, 181)
	slow := &slowNoUIDConn{Conn: f.p.conn(), delay: 800 * time.Millisecond, started: make(chan struct{})}
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		return slow, nil
	}
	rec := f.postMove(t, 10, `{"to":"inbox"}`)
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
	rec = f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo during flight status = %d, body %s", rec.Code, rec.Body.String())
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
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("original op status = %q, want done", o.Status)
	}
	if u := opUndone(t, f, opID); u != 1 {
		t.Fatal("the deferred undo never marked the original op undone")
	}
	route, err := f.st.MessageRoute(10)
	if err != nil {
		t.Fatal("message row is gone after the deferred undo drained; the restore-undo lost the message")
	}
	if route.FolderID != 3 {
		t.Fatalf("mirror folder after the deferred undo = %d, want 3 (Trash, back where it started)", route.FolderID)
	}
	if n := providerCount(f.p, "Trash"); n != 1 {
		t.Fatalf("provider Trash holds %d copies, want exactly 1", n)
	}
	if n := providerCount(f.p, "INBOX"); n != 0 {
		t.Fatalf("provider INBOX holds %d copies, want 0", n)
	}
	if _, ok := f.p.flags("Trash", route.UID); !ok {
		t.Fatal("mirror uid does not resolve to the provider copy in Trash")
	}
	var inverses int
	var invStatus string
	if err := f.st.DB.QueryRow("SELECT COUNT(*), COALESCE(MAX(status), '') FROM ops WHERE message_id = 10 AND id > ?", opID).
		Scan(&inverses, &invStatus); err != nil {
		t.Fatalf("count inverse ops: %v", err)
	}
	if inverses != 1 || invStatus != store.OpDone {
		t.Fatalf("inverse ops = %d (status %q), want exactly 1 done", inverses, invStatus)
	}
	if calls := f.p.callsOf("MoveMessage"); len(calls) != 2 {
		t.Fatalf("provider MoveMessage calls = %d, want 2 (restore out, undo back)", len(calls))
	}
}

func TestDeferredUndoFailureIsRecordedNotSilent(t *testing.T) {
	f := newDrainFixture(t)
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	f.p.missingErr = true
	f.p.storeFlagsDelay = 800 * time.Millisecond
	opID, _, err := f.st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		f.srv.drainAccount(nil, 1)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		o, err := f.st.GetOp(opID)
		if err == nil && o.Status == store.OpInFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain never claimed the flags op")
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := f.st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo of an in-flight op must register intent, not fail: %v", err)
	}
	if res.Mode != "deferred" {
		t.Fatalf("undo mode = %q, want deferred", res.Mode)
	}
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("drain did not finish")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done (the provider no longer has the message)", o.Status)
	}
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("the row of a provider-gone message should have been dropped by the flags outcome")
	}
	if o.UndoRequested {
		t.Fatal("the failed deferred undo left its intent dangling; nothing will ever process or report it")
	}
	if o.LastError == "" {
		t.Fatal("the failed deferred undo was recorded nowhere on the op; it vanished silently")
	}
	if u := opUndone(t, f, opID); u != 0 {
		t.Fatal("the failed deferred undo still marked the op undone")
	}
}

