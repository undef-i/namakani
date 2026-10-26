package store

import (
	"testing"
)

func TestUndoInFlightMoveRegistersDeferredIntent(t *testing.T) {
	st := opsTestStore(t)
	opID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", opID); err != nil {
		t.Fatalf("force in_flight: %v", err)
	}
	res, err := st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo of an in-flight op must register intent, not fail: %v", err)
	}
	if res.Mode != "deferred" {
		t.Fatalf("undo mode = %q, want deferred", res.Mode)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if !o.UndoRequested {
		t.Fatal("undo intent was not persisted on the op")
	}
	if o.Status != OpInFlight {
		t.Fatalf("op status = %q, want still in_flight (the drainer owns it)", o.Status)
	}
	if err := st.CompleteMoveOp(opID, 2, 10, 555); err != nil {
		t.Fatalf("complete move: %v", err)
	}
	res, err = st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo after completion: %v", err)
	}
	if res.Mode != "inverse" || res.Inverse == nil {
		t.Fatalf("undo after completion = %+v, want an inverse op", res)
	}
	route, err := st.MessageRoute(10)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.FolderID != 1 {
		t.Fatalf("mirror folder after inverse enqueue = %d, want 1 (INBOX)", route.FolderID)
	}
}

