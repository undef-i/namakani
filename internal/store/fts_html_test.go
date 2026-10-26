package store

import "testing"

func ftsMatchCount(t *testing.T, st *Store, term string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, `"`+term+`"`).Scan(&n); err != nil {
		t.Fatalf("match query: %v", err)
	}
	return n
}

func TestSaveBodyIndexesHTMLTextWhenNoPlainText(t *testing.T) {
	st := opsTestStore(t)
	if err := st.SaveBody(10, "", `<p>Hello <b>zebra</b> crossing</p><script>var s = "quixotic";</script>`, "Hello zebra", false); err != nil {
		t.Fatalf("save body: %v", err)
	}
	if n := ftsMatchCount(t, st, "zebra"); n != 1 {
		t.Fatalf("MATCH zebra = %d rows, want 1: an HTML-only body must be searchable by its text", n)
	}
	if n := ftsMatchCount(t, st, "quixotic"); n != 0 {
		t.Fatalf("MATCH quixotic = %d rows, want 0: script content is not message text", n)
	}
}

func TestBackfillFTSHTMLBodies(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.DB.Exec(`INSERT INTO bodies(message_id, text_body, html_body) VALUES(11, '', '<p>backfill <i>walrus</i> here</p>')`); err != nil {
		t.Fatalf("insert body: %v", err)
	}
	if err := st.ftsReplace(11, "Subj", "{}", "[]", ""); err != nil {
		t.Fatalf("seed fts row: %v", err)
	}
	if n := ftsMatchCount(t, st, "walrus"); n != 0 {
		t.Fatalf("precondition: MATCH walrus = %d, want 0 before backfill", n)
	}
	done, err := st.BackfillFTSHTMLBodies()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if done != 1 {
		t.Fatalf("backfilled = %d, want 1", done)
	}
	if n := ftsMatchCount(t, st, "walrus"); n != 1 {
		t.Fatalf("MATCH walrus = %d after backfill, want 1", n)
	}
	done, err = st.BackfillFTSHTMLBodies()
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if done != 0 {
		t.Fatalf("second backfill processed %d rows, want 0 (nothing left unindexed)", done)
	}
}
