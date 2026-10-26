package store

import "testing"

func TestUpdateMessageFlagsNeverRegressesToAnOlderModSeq(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.DB.Exec("UPDATE messages SET flags = 1, modseq = 100 WHERE id = 10"); err != nil {
		t.Fatalf("seed flags: %v", err)
	}
	read := func() (uint32, uint64) {
		t.Helper()
		var flags uint32
		var modseq uint64
		if err := st.DB.QueryRow("SELECT flags, modseq FROM messages WHERE id = 10").Scan(&flags, &modseq); err != nil {
			t.Fatalf("read row: %v", err)
		}
		return flags, modseq
	}
	changed, err := st.UpdateMessageFlags(1, 100, 0, 99)
	if err != nil {
		t.Fatalf("stale write: %v", err)
	}
	if changed {
		t.Fatal("a flags observation from modseq 99 must not overwrite state already known at modseq 100")
	}
	if flags, modseq := read(); flags != 1 || modseq != 100 {
		t.Fatalf("row = flags %d modseq %d after stale write, want flags 1 modseq 100", flags, modseq)
	}
	changed, err = st.UpdateMessageFlags(1, 100, 0, 101)
	if err != nil {
		t.Fatalf("fresh write: %v", err)
	}
	if !changed {
		t.Fatal("a flags observation from modseq 101 must apply")
	}
	if flags, modseq := read(); flags != 0 || modseq != 101 {
		t.Fatalf("row = flags %d modseq %d after fresh write, want flags 0 modseq 101", flags, modseq)
	}
	changed, err = st.UpdateMessageFlags(1, 100, 1, 0)
	if err != nil {
		t.Fatalf("modseq-less write: %v", err)
	}
	if !changed {
		t.Fatal("a server without modseqs must still have its flag writes applied")
	}
	if flags, _ := read(); flags != 1 {
		t.Fatalf("row flags = %d after modseq-less write, want 1", flags)
	}
}
