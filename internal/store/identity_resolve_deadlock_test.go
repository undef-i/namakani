package store

import (
	"testing"
	"time"
)

// Regression: candidateRows resolved 'p:'/'t:' identities by issuing a
// second query on the same *sql.DB from inside the iteration of the first
// query's rows. With the store's single connection that self-deadlocks:
// the rows hold the only connection while the inner query waits for it.
// The resolution must finish, and the store must stay usable afterwards.
func TestResolveProtonIdentityDoesNotDeadlock(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 20, 2, 5, 55, "ignored@x")
	if _, err := st.DB.Exec(`INSERT INTO proton_uids(account_id, folder_name, uid, proton_id) VALUES(2, 'INBOX', 55, 'P123')`); err != nil {
		t.Fatalf("proton uids: %v", err)
	}
	opID, err := st.EnqueueExpunge(2, 20, "")
	if err != nil {
		t.Fatalf("enqueue proton expunge: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Identity != "p:P123" {
		t.Fatalf("identity = %q, want p:P123", o.Identity)
	}

	type result struct {
		target ResolvedTarget
		ok     bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		target, ok, err := st.ResolveOpTarget(o)
		done <- result{target, ok, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("resolve: %v", r.err)
		}
		if !r.ok || r.target.RowID != 20 {
			t.Fatalf("resolve = %+v ok=%v, want row 20", r.target, r.ok)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ResolveOpTarget did not return within 15s: connection self-deadlock")
	}

	// The single connection must be back: any later store query completes.
	probe := make(chan error, 1)
	go func() {
		_, _, err := st.GetSetting("no_such_key")
		probe <- err
	}()
	select {
	case err := <-probe:
		if err != nil {
			t.Fatalf("post-resolve query: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("store unusable after resolve: connection not returned")
	}
}

// Same shape for the Tuta mapping table.
func TestResolveTutaIdentityDoesNotDeadlock(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 30, 3, 6, 77, "ignored@x")
	if _, err := st.DB.Exec(`INSERT INTO tuta_uids(account_id, folder_name, uid, tuta_id) VALUES(3, 'INBOX', 77, 'T9')`); err != nil {
		t.Fatalf("tuta uids: %v", err)
	}
	opID, err := st.EnqueueExpunge(3, 30, "")
	if err != nil {
		t.Fatalf("enqueue tuta expunge: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Identity != "t:T9" {
		t.Fatalf("identity = %q, want t:T9", o.Identity)
	}
	type result struct {
		target ResolvedTarget
		ok     bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		target, ok, err := st.ResolveOpTarget(o)
		done <- result{target, ok, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("resolve: %v", r.err)
		}
		if !r.ok || r.target.RowID != 30 {
			t.Fatalf("resolve = %+v ok=%v, want row 30", r.target, r.ok)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ResolveOpTarget did not return within 15s: connection self-deadlock")
	}
}
