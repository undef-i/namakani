package store

import (
	"errors"
	"testing"
	"time"
)

func opUndoneFlag(t *testing.T, st *Store, id int64) int {
	t.Helper()
	var u int
	if err := st.DB.QueryRow("SELECT undone FROM ops WHERE id = ?", id).Scan(&u); err != nil {
		t.Fatalf("read undone for op %d: %v", id, err)
	}
	return u
}

func TestUndoDoneMoveWithUnresolvableInverseLeavesOpUnmarked(t *testing.T) {
	st := opsTestStore(t)
	opID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if _, err := st.ClaimDueOps(1, time.Now().Unix(), 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteMoveOp(opID, 2, 10, 555); err != nil {
		t.Fatalf("complete move: %v", err)
	}
	if _, err := st.EnqueueExpunge(1, 10, ""); err != nil {
		t.Fatalf("queue an expunge behind the move so the inverse move is blocked: %v", err)
	}
	if _, err := st.UndoOp(opID); err == nil {
		t.Fatal("undo must fail when the inverse move cannot be enqueued")
	}
	if u := opUndoneFlag(t, st, opID); u != 0 {
		t.Fatal("a failed undo marked the op undone; that half-state is the production incident")
	}
	route, err := st.MessageRoute(10)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.FolderID != 2 || route.UID != 555 {
		t.Fatalf("mirror after the failed undo = folder %d uid %d, want folder 2 uid 555 (message untouched)", route.FolderID, route.UID)
	}
}

func TestUndoDoneFlagsWithMissingMessageLeavesOpUnmarked(t *testing.T) {
	st := opsTestStore(t)
	opID, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	if _, err := st.ClaimDueOps(1, time.Now().Unix(), 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteOp(opID); err != nil {
		t.Fatalf("complete flags op: %v", err)
	}
	if _, err := st.DB.Exec("DELETE FROM messages WHERE id = 10"); err != nil {
		t.Fatalf("drop the message row: %v", err)
	}
	if _, err := st.UndoOp(opID); err == nil {
		t.Fatal("undo must fail when the message row it depends on is gone")
	}
	if u := opUndoneFlag(t, st, opID); u != 0 {
		t.Fatal("a failed undo marked the op undone; that half-state is the production incident")
	}
}

func TestUndoDoneExpungeCannotUndoAndStaysUnmarked(t *testing.T) {
	st := opsTestStore(t)
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if _, err := st.ClaimDueOps(1, time.Now().Unix(), 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteOp(opID); err != nil {
		t.Fatalf("complete expunge: %v", err)
	}
	if _, err := st.UndoOp(opID); !errors.Is(err, ErrOpCannotUndo) {
		t.Fatalf("undo of a done expunge = %v, want ErrOpCannotUndo", err)
	}
	if u := opUndoneFlag(t, st, opID); u != 0 {
		t.Fatal("an expunge that cannot be undone was still marked undone")
	}
}
