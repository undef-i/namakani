package store

import (
	"testing"
	"time"
)

func claimedFlagsOp(t *testing.T, st *Store) *Op {
	t.Helper()
	id, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	ops, err := st.ClaimDueOps(1, time.Now().Unix(), 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, o := range ops {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("claimed ops do not contain %d", id)
	return nil
}

func TestUndoRequestDiesWhenOpParks(t *testing.T) {
	st := opsTestStore(t)
	o := claimedFlagsOp(t, st)
	res, err := st.UndoOp(o.ID)
	if err != nil {
		t.Fatalf("undo while in flight: %v", err)
	}
	if res.Mode != "deferred" {
		t.Fatalf("undo mode = %q, want deferred", res.Mode)
	}
	status, err := st.FailOp(o.ID, "auth failed", true, time.Now().Unix())
	if err != nil {
		t.Fatalf("fail op: %v", err)
	}
	if status != OpNeedsAttention {
		t.Fatalf("status = %q, want needs_attention", status)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.UndoRequested {
		t.Fatal("undo_requested survived parking; the stale intent must die when the op leaves the execution pipeline, or a later retry will execute a forgotten undo")
	}
}

func TestUndoRequestSurvivesRetryableFailure(t *testing.T) {
	st := opsTestStore(t)
	o := claimedFlagsOp(t, st)
	if _, err := st.UndoOp(o.ID); err != nil {
		t.Fatalf("undo while in flight: %v", err)
	}
	status, err := st.FailOp(o.ID, "dial timeout", false, time.Now().Unix())
	if err != nil {
		t.Fatalf("fail op: %v", err)
	}
	if status != OpPending {
		t.Fatalf("status = %q, want pending", status)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if !cur.UndoRequested {
		t.Fatal("undo_requested was cleared by a retryable failure; the op is still in the pipeline, so the registered intent must live until the op completes or parks")
	}
}

func TestRetryOpVoidsStaleUndoRequest(t *testing.T) {
	st := opsTestStore(t)
	id, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'needs_attention', undo_requested = 1 WHERE id = ?", id); err != nil {
		t.Fatalf("park op with a stale undo request: %v", err)
	}
	if err := st.RetryOp(id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	cur, err := st.GetOp(id)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.Status != OpPending {
		t.Fatalf("status = %q, want pending", cur.Status)
	}
	if cur.UndoRequested {
		t.Fatal("retry kept a stale undo_requested; when the retried op completes, a forgotten undo would fire")
	}
}
