package store

import (
	"testing"
	"time"
)

func claimSendForSeal(t *testing.T, st *Store) *Op {
	t.Helper()
	opID, err := st.EnqueueSend(1, `{}`, 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	ops, err := st.ClaimDueOps(1, time.Now().Unix(), 10, OpSend)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(ops) != 1 || ops[0].ID != opID {
		t.Fatalf("claimed %d ops, want the send op", len(ops))
	}
	if err := st.StampClaimToken([]int64{opID}, 42); err != nil {
		t.Fatalf("stamp token: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != OpInFlight || o.ClaimToken != 42 {
		t.Fatalf("claimed op = %+v, want in_flight with token 42", o)
	}
	return o
}

func TestSealSendForDeliveryWithoutUndoSealsAndRotatesToken(t *testing.T) {
	st := identityTestStore(t)
	o := claimSendForSeal(t, st)
	sealed, undo, err := st.SealSendForDelivery(o.ID, 42, 43)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !sealed || undo {
		t.Fatalf("seal = (%v, %v), want (true, false)", sealed, undo)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.ClaimToken != 43 {
		t.Fatalf("claim token after seal = %d, want 43 (the fence moved to the executor)", cur.ClaimToken)
	}
	sealed, _, err = st.SealSendForDelivery(o.ID, 42, 44)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if sealed {
		t.Fatal("a second seal under the pre-seal token succeeded; the gate must admit exactly one crossing")
	}
}

func TestSealSendForDeliveryAfterUndoRequestRefuses(t *testing.T) {
	st := identityTestStore(t)
	o := claimSendForSeal(t, st)
	res, err := st.UndoOp(o.ID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if res.Mode != "deferred" {
		t.Fatalf("undo of a claimed send mode = %q, want deferred", res.Mode)
	}
	sealed, undo, err := st.SealSendForDelivery(o.ID, 42, 43)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed || !undo {
		t.Fatalf("seal after undo request = (%v, %v), want (false, true): an undo that reached the store before the gate must win", sealed, undo)
	}
}

func TestSealSendForDeliveryTakesUnclaimedOp(t *testing.T) {
	st := identityTestStore(t)
	opID, err := st.EnqueueSend(1, `{}`, 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	sealed, undo, err := st.SealSendForDelivery(opID, 0, 7)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !sealed || undo {
		t.Fatalf("seal of an unclaimed op = (%v, %v), want (true, false)", sealed, undo)
	}
	cur, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.Status != OpInFlight || cur.ClaimToken != 7 {
		t.Fatalf("op after take-over seal = (%q, token %d), want (in_flight, 7)", cur.Status, cur.ClaimToken)
	}
}

func TestSealSendForDeliveryRefusesUnclaimedOpWithUndoFlag(t *testing.T) {
	st := identityTestStore(t)
	opID, err := st.EnqueueSend(1, `{}`, 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET undo_requested = 1 WHERE id = ?", opID); err != nil {
		t.Fatalf("set undo flag: %v", err)
	}
	sealed, undo, err := st.SealSendForDelivery(opID, 0, 7)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed || !undo {
		t.Fatalf("seal of an unclaimed op carrying an undo request = (%v, %v), want (false, true)", sealed, undo)
	}
}
