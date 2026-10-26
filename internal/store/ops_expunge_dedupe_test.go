package store

import "testing"

func TestEnqueueExpungeDedupesAcrossAllActiveStates(t *testing.T) {
	st := opsTestStore(t)
	folderTotal := func() int {
		t.Helper()
		var total int
		if err := st.DB.QueryRow("SELECT total FROM folders WHERE id = 1").Scan(&total); err != nil {
			t.Fatalf("folder total: %v", err)
		}
		return total
	}
	if _, err := st.DB.Exec("UPDATE folders SET total = 10 WHERE id = 1"); err != nil {
		t.Fatalf("seed total: %v", err)
	}
	first, err := st.EnqueueExpunge(1, 11, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := folderTotal(); got != 9 {
		t.Fatalf("folder total after first enqueue = %d, want 9", got)
	}
	for _, status := range []string{OpInFlight, OpNeedsAttention, OpPending} {
		if _, err := st.DB.Exec("UPDATE ops SET status = ? WHERE id = ?", status, first); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
		again, err := st.EnqueueExpunge(1, 11, "")
		if err != nil {
			t.Fatalf("re-enqueue while %s: %v", status, err)
		}
		if again != first {
			t.Fatalf("re-enqueue while %s created twin op %d, want the existing op %d", status, again, first)
		}
		if got := folderTotal(); got != 9 {
			t.Fatalf("folder total after re-enqueue while %s = %d, want 9: the count may only move when a row is actually inserted", status, got)
		}
	}
}
