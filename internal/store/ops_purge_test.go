package store

import (
	"testing"
	"time"
)

func purgeFixtureOp(t *testing.T, st *Store, typ string, createdAgo, finishedAgo int64) int64 {
	t.Helper()
	now := time.Now().Unix()
	res, err := st.DB.Exec(`INSERT INTO ops(account_id, type, message_id, payload, status, created_at, finished_at)
		VALUES(1, ?, 10, '{}', 'done', ?, ?)`, typ, now-createdAgo, now-finishedAgo)
	if err != nil {
		t.Fatalf("insert op: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func opExists(t *testing.T, st *Store, id int64) bool {
	t.Helper()
	var n int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count op: %v", err)
	}
	return n == 1
}

func TestPurgeTerminalOpsUsesFinishTimeNotCreationTime(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	old, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET created_at = ? WHERE id = ?", now-3*86400, old); err != nil {
		t.Fatalf("age op: %v", err)
	}
	if _, err := st.ClaimDueOps(1, now, 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteOp(old); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !opExists(t, st, old) {
		t.Fatalf("op created 3 days ago but finished just now was purged: the retention window must run from when the op finished, or long-scheduled work loses its undo and audit window the moment it completes")
	}
	if _, err := st.DB.Exec("UPDATE ops SET finished_at = ? WHERE id = ?", now-3601, old); err != nil {
		t.Fatalf("age finish: %v", err)
	}
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge 2: %v", err)
	}
	if opExists(t, st, old) {
		t.Fatalf("op finished over an hour ago was not purged")
	}
}

func TestPurgeTerminalOpsKeepsSendAuditLonger(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	send := purgeFixtureOp(t, st, OpSend, 8*86400, 2*86400)
	move := purgeFixtureOp(t, st, OpMove, 8*86400, 2*86400)
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !opExists(t, st, send) {
		t.Fatalf("send op finished 2 days ago was purged: sent-mail audit and already-sent checks need a longer window")
	}
	if opExists(t, st, move) {
		t.Fatalf("move op finished 2 days ago was kept, want purged")
	}
}
