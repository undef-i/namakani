package store

import "testing"

func TestEnqueueCapturesFolderUIDValidity(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.DB.Exec("UPDATE folders SET uidvalidity = 7 WHERE id = 1"); err != nil {
		t.Fatalf("set validity: %v", err)
	}
	moveID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	flagsID, _, err := st.EnqueueFlags(1, 11, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	expungeID, err := st.EnqueueExpunge(1, 11, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	for _, id := range []int64{moveID, flagsID, expungeID} {
		o, err := st.GetOp(id)
		if err != nil {
			t.Fatalf("get op %d: %v", id, err)
		}
		if o.PriorUIDValidity != 7 {
			t.Fatalf("op %d (%s) captured uidvalidity %d, want 7: an op must carry the epoch of the folder it will act on", id, o.Type, o.PriorUIDValidity)
		}
	}
}
