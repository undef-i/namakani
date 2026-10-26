package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

// A drain must not hold claims while it is still waiting for its provider
// connection: an op may only become in_flight once the wave can execute it.
func TestDrainDoesNotClaimWhileConnOpenBlocks(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("move handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)

	openEntered := make(chan struct{})
	openRelease := make(chan struct{})
	var openOnce sync.Once
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		openOnce.Do(func() { close(openEntered) })
		<-openRelease
		return f.p.conn(), nil
	}
	drained := make(chan struct{})
	go func() {
		f.srv.drainAccount(nil, 1)
		close(drained)
	}()
	select {
	case <-openEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never tried to open its connection")
	}
	// Give the drain every chance to claim early; the op must stay pending
	// for as long as the connection is not there yet.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		o, err := f.st.GetOp(opID)
		if err != nil {
			t.Fatalf("get op: %v", err)
		}
		if o.Status != store.OpPending {
			t.Fatalf("op status while conn open is blocked = %q, want pending (claims happen only with the connection in hand)", o.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(openRelease)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after the connection opened")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status after drain = %q, want done", o.Status)
	}
	if n := len(f.p.callsOf("MoveMessage")); n != 1 {
		t.Fatalf("provider MoveMessage calls = %d, want 1", n)
	}
}

type hangConn struct {
	*fakeConn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Int32
}

func (c *hangConn) MoveMessage(uid uint32, dest string) (uint32, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return 0, errors.New("connection torn down under the hung call")
}

func (c *hangConn) Close() {
	c.closed.Add(1)
	c.fakeConn.Close()
}

// A wave whose executor hangs after claiming must lose its claims to the
// watchdog: the ops return to pending, the hung goroutine's late landing is
// fenced by its claim token, and a fresh drain applies the upstream action
// exactly once.
func TestStuckWaveIsKilledReclaimedAndLandsOnce(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	f.srv.claimStuckSecs = 1

	hc := &hangConn{fakeConn: &fakeConn{p: f.p}, entered: make(chan struct{}), release: make(chan struct{})}
	var openCalls atomic.Int32
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		if openCalls.Add(1) == 1 {
			return hc, nil
		}
		return f.p.conn(), nil
	}
	drained := make(chan struct{})
	go func() {
		f.srv.drainAccount(nil, 1)
		close(drained)
	}()
	select {
	case <-hc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wave never reached the hanging provider call")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpInFlight {
		t.Fatalf("op status while the wave is hung = %q, want in_flight", o.Status)
	}
	if o.ClaimToken == 0 {
		t.Fatal("claimed op carries no claim token")
	}
	// While the wave is registered as the live owner, the reaper must not
	// touch its claims.
	f.srv.reclaimOrphanedClaims()
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpInFlight {
		t.Fatalf("reaper released a live wave's claim: status = %q, want in_flight", o.Status)
	}
	// The watchdog declares the wave dead after its progress budget lapses:
	// registration is dropped and the connection is torn down.
	f.srv.killStuckWaves(time.Now().Unix() + f.srv.claimStuckSecs + 1)
	if hc.closed.Load() == 0 {
		t.Fatal("watchdog did not close the stuck wave's connection")
	}
	f.srv.reclaimOrphanedClaims()
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpPending {
		t.Fatalf("op status after reclaim = %q, want pending", o.Status)
	}
	if o.ClaimToken != 0 {
		t.Fatalf("reclaimed op still carries claim token %d, want 0", o.ClaimToken)
	}
	// Let the hung call return. The zombie wave's landing is fenced by the
	// token: the op must stay pending with zero attempts recorded.
	close(hc.release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("zombie drain did not exit after being declared dead")
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpPending {
		t.Fatalf("zombie landing changed the op: status = %q, want pending", o.Status)
	}
	if o.Attempts != 0 {
		t.Fatalf("zombie landing recorded attempts = %d, want 0 (its FailOp must be fenced)", o.Attempts)
	}
	// A fresh drain executes the op exactly once upstream.
	f.srv.drainAccount(nil, 1)
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after the healing drain = %q, want done", o.Status)
	}
	if n := len(f.p.callsOf("MoveMessage")); n != 1 {
		t.Fatalf("provider MoveMessage applications = %d, want exactly 1", n)
	}
}

// An in_flight op with no live owner anywhere (the process restarted, the
// claiming wave is gone) is reclaimed by the sweep reaper even when the
// account has no pending op for AccountsWithDueOps to enumerate.
func TestReaperReclaimsOwnerlessInFlight(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", opID); err != nil {
		t.Fatalf("strand op in_flight: %v", err)
	}
	f.srv.reclaimOrphanedClaims()
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpPending {
		t.Fatalf("ownerless in_flight op after reclaim = %q, want pending", o.Status)
	}
	f.srv.drainAccount(nil, 1)
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("reclaimed op status after drain = %q, want done", o.Status)
	}
	if n := len(f.p.callsOf("StoreFlags")); n != 1 {
		t.Fatalf("provider StoreFlags calls = %d, want 1", n)
	}
}
