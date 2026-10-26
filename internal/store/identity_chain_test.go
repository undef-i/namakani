package store

import "testing"

// A pending expunge from a previous incarnation must block a move issued
// against the rebuilt row: the chain follows the message, not the row.
func TestCrossIncarnationExpungeBlocksMove(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	if _, err := st.EnqueueExpunge(1, 10, ""); err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	if _, err := st.EnqueueMove(1, 77, 2, ""); err != ErrOpBlocked {
		t.Fatalf("move against rebuilt row = %v, want ErrOpBlocked (identity chain)", err)
	}
}

// A second expunge against the rebuilt row, same folder, dedupes onto the
// op from the previous incarnation instead of creating a second delete.
func TestCrossIncarnationExpungeDedupesSameFolder(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	first, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	second, err := st.EnqueueExpunge(1, 77, "")
	if err != nil {
		t.Fatalf("enqueue expunge on rebuilt row: %v", err)
	}
	if second != first {
		t.Fatalf("second expunge = op %d, want dedupe onto op %d", second, first)
	}
}

// The same Message-ID in another folder is a different physical copy: its
// expunge must not dedupe onto the first folder's op.
func TestCrossFolderTwinExpungeDoesNotDedupe(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "dup@x")
	insertRow(t, st, 11, 1, 2, 100, "dup@x")
	first, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue expunge inbox copy: %v", err)
	}
	second, err := st.EnqueueExpunge(1, 11, "")
	if err != nil {
		t.Fatalf("enqueue expunge archive copy: %v", err)
	}
	if second == first {
		t.Fatalf("archive copy deduped onto inbox op %d; distinct copies need distinct ops", first)
	}
}

// Flags intents coalesce only within one incarnation: a flags op against
// the rebuilt row must not merge into the dead incarnation's op.
func TestFlagsDoNotMergeAcrossIncarnations(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	seen := true
	first, _, err := st.EnqueueFlags(1, 10, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	if first == 0 {
		t.Fatal("first flags op id = 0")
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	unseen := false
	second, _, err := st.EnqueueFlags(1, 77, &unseen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags on rebuilt row: %v", err)
	}
	if second == first || second == 0 {
		t.Fatalf("flags on rebuilt row = op %d, want a fresh op (first was %d)", second, first)
	}
}

// ClaimDueOps serializes by identity: a fresh op on the rebuilt row waits
// behind the still-pending op from the previous incarnation.
func TestClaimDueOpsSerializesByIdentity(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	seen := true
	first, _, err := st.EnqueueFlags(1, 10, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	unseen := false
	second, _, err := st.EnqueueFlags(1, 77, &unseen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags 2: %v", err)
	}
	claimed, err := st.ClaimDueOps(1, 9999999999, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != first {
		ids := []int64{}
		for _, o := range claimed {
			ids = append(ids, o.ID)
		}
		t.Fatalf("claimed = %v, want only the older op %d (op %d must wait its turn)", ids, first, second)
	}
}

// Undo of a completed op resolves the message's current incarnation even
// after the acted row was replaced.
func TestUndoResolvesCurrentIncarnation(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	seen := true
	opID, _, err := st.EnqueueFlags(1, 10, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'done' WHERE id = ?", opID); err != nil {
		t.Fatalf("complete op: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	res, err := st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if res.Inverse == nil {
		t.Fatal("undo returned no inverse op")
	}
	if res.Inverse.MessageID != 77 {
		t.Fatalf("inverse op targets row %d, want rebuilt row 77", res.Inverse.MessageID)
	}
}
