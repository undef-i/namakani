package store

import (
	"errors"
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

func TestCancelInFlightSendOnlyAppliesToInFlightSends(t *testing.T) {
	st := opsTestStore(t)
	sendID, err := st.EnqueueSend(1, "{}", 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	if err := st.CancelInFlightSend(sendID); !errors.Is(err, ErrOpNotActive) {
		t.Fatalf("cancel a pending send = %v, want ErrOpNotActive (only in-flight sends)", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", sendID); err != nil {
		t.Fatalf("force in_flight: %v", err)
	}
	if err := st.CancelInFlightSend(sendID); err != nil {
		t.Fatalf("cancel in-flight send: %v", err)
	}
	o, _ := st.GetOp(sendID)
	if o.Status != OpCancelled {
		t.Fatalf("send status = %q, want cancelled", o.Status)
	}
	moveID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", moveID); err != nil {
		t.Fatalf("force move in_flight: %v", err)
	}
	if err := st.CancelInFlightSend(moveID); !errors.Is(err, ErrOpNotActive) {
		t.Fatalf("cancelling an in-flight move via the send path = %v, want ErrOpNotActive", err)
	}
}
