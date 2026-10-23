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
