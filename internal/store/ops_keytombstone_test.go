package store

import (
	"errors"
	"testing"
	"time"
)

func finishAndAgeOp(t *testing.T, st *Store, id int64, finishedAgo int64) {
	t.Helper()
	now := time.Now().Unix()
	claimed, err := st.ClaimDueOps(1, now, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	found := false
	for _, o := range claimed {
		if o.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("op %d was not claimed", id)
	}
	if err := st.CompleteOp(id); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET finished_at = ? WHERE id = ?", now-finishedAgo, id); err != nil {
		t.Fatalf("age finish: %v", err)
	}
}

func countOps(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM ops").Scan(&n); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	return n
}

func TestClientKeyResolvesAfterOpRowPurged(t *testing.T) {
	now := time.Now().Unix()
	t.Run("flags", func(t *testing.T) {
		st := opsTestStore(t)
		seen := true
		id, _, err := st.EnqueueFlags(1, 10, &seen, nil, "tk-flags")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		finishAndAgeOp(t, st, id, 3601)
		if err := st.PurgeTerminalOps(now); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if opExists(t, st, id) {
			t.Fatal("op row should be purged")
		}
		again, _, err := st.EnqueueFlags(1, 10, &seen, nil, "tk-flags")
		if err != nil || again != id {
			t.Fatalf("replay after purge = (%d, %v), want (%d, nil)", again, err, id)
		}
		if n := countOps(t, st); n != 0 {
			t.Fatalf("replay created %d new ops, want 0", n)
		}
		unseen := false
		if _, _, err := st.EnqueueFlags(1, 10, &unseen, nil, "tk-flags"); !errors.Is(err, ErrClientKeyConflict) {
			t.Fatalf("conflicting replay after purge: err = %v, want ErrClientKeyConflict", err)
		}
	})
	t.Run("move", func(t *testing.T) {
		st := opsTestStore(t)
		id, err := st.EnqueueMove(1, 10, 2, "tk-move")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		claimed, err := st.ClaimDueOps(1, now, 10)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (%d ops)", err, len(claimed))
		}
		if err := st.CompleteMoveOp(id, 2, 10, 555); err != nil {
			t.Fatalf("complete move: %v", err)
		}
		if _, err := st.DB.Exec("UPDATE ops SET finished_at = ? WHERE id = ?", now-3601, id); err != nil {
			t.Fatalf("age finish: %v", err)
		}
		back, err := st.EnqueueMove(1, 10, 1, "")
		if err != nil {
			t.Fatalf("move back: %v", err)
		}
		finishAndAgeOp(t, st, back, 0)
		if err := st.PurgeTerminalOps(now); err != nil {
			t.Fatalf("purge: %v", err)
		}
		again, err := st.EnqueueMove(1, 10, 2, "tk-move")
		if err != nil || again != id {
			t.Fatalf("replay after purge = (%d, %v), want (%d, nil)", again, err, id)
		}
		route, err := st.MessageRoute(10)
		if err != nil {
			t.Fatalf("route: %v", err)
		}
		if route.FolderID != 1 {
			t.Fatalf("replay re-applied the move: message is in folder %d, want 1 (the replay must resolve to the old op, not move again)", route.FolderID)
		}
		if _, err := st.EnqueueMove(1, 10, 3, "tk-move"); !errors.Is(err, ErrClientKeyConflict) {
			t.Fatalf("conflicting replay after purge: err = %v, want ErrClientKeyConflict", err)
		}
	})
	t.Run("expunge", func(t *testing.T) {
		st := opsTestStore(t)
		id, err := st.EnqueueExpunge(1, 10, "tk-expunge")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		finishAndAgeOp(t, st, id, 3601)
		if err := st.PurgeTerminalOps(now); err != nil {
			t.Fatalf("purge: %v", err)
		}
		again, err := st.EnqueueExpunge(1, 10, "tk-expunge")
		if err != nil || again != id {
			t.Fatalf("replay after purge = (%d, %v), want (%d, nil)", again, err, id)
		}
		if n := countOps(t, st); n != 0 {
			t.Fatalf("replay created %d new ops, want 0", n)
		}
	})
	t.Run("send", func(t *testing.T) {
		st := opsTestStore(t)
		id, err := st.EnqueueSend(1, `{"v":1}`, 0, "tk-send")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		finishAndAgeOp(t, st, id, 8*86400)
		if err := st.PurgeTerminalOps(now); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if opExists(t, st, id) {
			t.Fatal("send op row finished 8 days ago should be purged")
		}
		again, err := st.EnqueueSend(1, `{"v":1}`, 0, "tk-send")
		if err != nil || again != id {
			t.Fatalf("send replay after row purge = (%d, %v), want (%d, nil): a resent send request must never produce a second send", again, err, id)
		}
		if n := countOps(t, st); n != 0 {
			t.Fatalf("replay created %d new ops, want 0", n)
		}
		if _, err := st.EnqueueSend(1, `{"v":2}`, 0, "tk-send"); !errors.Is(err, ErrClientKeyConflict) {
			t.Fatalf("conflicting send replay after purge: err = %v, want ErrClientKeyConflict", err)
		}
	})
}

func TestClientKeyTombstoneExpiresAfterRetention(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	seen := true
	id, _, err := st.EnqueueFlags(1, 10, &seen, nil, "tk-expire")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	finishAndAgeOp(t, st, id, 3601)
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE op_client_keys SET expires_at = ? WHERE op_id = ?", now-1, id); err != nil {
		t.Fatalf("age tombstone: %v", err)
	}
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge 2: %v", err)
	}
	again, _, err := st.EnqueueFlags(1, 10, &seen, nil, "tk-expire")
	if err != nil {
		t.Fatalf("replay after tombstone expiry: %v", err)
	}
	if again == id || again == 0 {
		t.Fatalf("replay after tombstone expiry = %d, want a fresh op id (the key is forgotten once its week is over)", again)
	}
}

func TestMergedFlagsKeyResolvesAfterPurge(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	seen := true
	flagged := true
	first, _, err := st.EnqueueFlags(1, 10, &seen, nil, "mk-1")
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	merged, _, err := st.EnqueueFlags(1, 10, nil, &flagged, "mk-2")
	if err != nil {
		t.Fatalf("enqueue merged: %v", err)
	}
	if merged != first {
		t.Fatalf("merged enqueue = %d, want the tail op %d", merged, first)
	}
	finishAndAgeOp(t, st, first, 3601)
	if err := st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge: %v", err)
	}
	again, _, err := st.EnqueueFlags(1, 10, nil, &flagged, "mk-2")
	if err != nil || again != first {
		t.Fatalf("merged key replay after purge = (%d, %v), want (%d, nil)", again, err, first)
	}
	againFirst, _, err := st.EnqueueFlags(1, 10, &seen, nil, "mk-1")
	if err != nil || againFirst != first {
		t.Fatalf("first key replay after purge = (%d, %v), want (%d, nil)", againFirst, err, first)
	}
	if n := countOps(t, st); n != 0 {
		t.Fatalf("replays created %d new ops, want 0", n)
	}
}
