package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestDrainConnectFailureBacksOffPerAccount(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	seen := true
	opID, _, err := f.st.EnqueueFlags(1, 10, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	calls := 0
	f.srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		calls++
		return nil, errors.New("dial refused")
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if calls != 1 {
		t.Fatalf("connect attempts = %d after 3 drain passes, want 1: a dead account must back off instead of being retried every sweep", calls)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpPending || o.Attempts != 0 {
		t.Fatalf("op after connect failure = %s attempts %d, want pending with 0 attempts (the op itself did nothing wrong)", o.Status, o.Attempts)
	}
	f.srv.connFailMu.Lock()
	st := f.srv.connFail[1]
	if st != nil {
		st.until = time.Now().Unix() - 1
	}
	f.srv.connFailMu.Unlock()
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if calls != 2 {
		t.Fatalf("connect attempts after backoff expiry = %d, want 2", calls)
	}
}
