package store

import (
	"path/filepath"
	"testing"
)

func TestThreadRepairRowsDrainAfterMarkChecked(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name) VALUES(1, 1, 'Inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key) VALUES
		(1, 1, 1, 11, 'm1@x', 'm1@x'),
		(2, 1, 1, 12, 'm2@x', 'root@x'),
		(3, 1, 1, 13, 'm3@x', '')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	rows, err := st.ThreadRepairRows(1)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("candidates = %d, want 2 (self-keyed and empty-key rows)", len(rows))
	}
	if err := st.MarkThreadChecked([]int64{rows[0].ID, rows[1].ID}); err != nil {
		t.Fatalf("mark checked: %v", err)
	}
	rows, err = st.ThreadRepairRows(1)
	if err != nil {
		t.Fatalf("candidates after mark: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("candidates after mark = %d, want 0 (checked rows must drain)", len(rows))
	}
	if err := st.MarkThreadChecked(nil); err != nil {
		t.Fatalf("mark empty: %v", err)
	}
}

func TestNormalizeSubject(t *testing.T) {
	cases := map[string]string{
		"Hello":              "hello",
		"Re: Hello":          "hello",
		"RE: Re: Hello World": "hello world",
		"Fwd: Hello":         "hello",
		"Re[2]: Hello":       "hello",
		"Re: [list] Hello":   "[list] hello",
		"回复：你好 世界":           "你好 世界",
		"Changed":            "changed",
	}
	for in, want := range cases {
		if got := normalizeSubject(in); got != want {
			t.Fatalf("normalizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThreadKeySubjectFork(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name) VALUES(1, 1, 'Inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	ka := st.ComputeThreadKey(1, "a@x", "", "Hello")
	kb := st.ComputeThreadKey(1, "b@x", "a@x", "Re: Hello")
	if ka != "a@x" || kb != "a@x" {
		t.Fatalf("same-subject chain keys = %q, %q; want a@x, a@x", ka, kb)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key, subject) VALUES
		(1, 1, 1, 11, 'a@x', 'a@x', 'Hello'),
		(2, 1, 1, 12, 'b@x', 'a@x', 'Re: Hello')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	kc := st.ComputeThreadKey(1, "c@x", "b@x", "Totally different")
	if kc != "c@x" {
		t.Fatalf("subject change key = %q, want fork c@x", kc)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key, subject) VALUES
		(3, 1, 1, 13, 'c@x', 'c@x', 'Totally different')`); err != nil {
		t.Fatalf("insert c: %v", err)
	}
	kd := st.ComputeThreadKey(1, "d@x", "c@x", "Re: Totally different")
	if kd != "c@x" {
		t.Fatalf("fork descendant key = %q, want c@x", kd)
	}
	if err := st.RecomputeThreadKey(1, 3, []string{"a@x", "b@x"}, "b@x"); err != nil {
		t.Fatalf("recompute c: %v", err)
	}
	var got string
	if err := st.DB.QueryRow("SELECT thread_key FROM messages WHERE id = 3").Scan(&got); err != nil {
		t.Fatalf("scan c: %v", err)
	}
	if got != "c@x" {
		t.Fatalf("recomputed c key = %q, want c@x (fork preserved, no fallback to chain root)", got)
	}
	if err := st.DB.QueryRow("SELECT thread_key FROM messages WHERE id = 2").Scan(&got); err != nil {
		t.Fatalf("scan b: %v", err)
	}
	if got != "a@x" {
		t.Fatalf("b key after c recompute = %q, want a@x (sweep must not drag other subjects)", got)
	}
	if err := st.RecomputeThreadKey(1, 2, []string{"a@x"}, "a@x"); err != nil {
		t.Fatalf("recompute b: %v", err)
	}
	if err := st.DB.QueryRow("SELECT thread_key FROM messages WHERE id = 2").Scan(&got); err != nil {
		t.Fatalf("scan b2: %v", err)
	}
	if got != "a@x" {
		t.Fatalf("recomputed b key = %q, want a@x", got)
	}
}
