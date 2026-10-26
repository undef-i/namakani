package store

import (
	"path/filepath"
	"testing"
)

func TestWipeFolderLeavesOpsUntouched(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	for _, m := range []struct{ id, folder, uid int }{{1, 1, 41}, {2, 1, 42}, {3, 2, 43}} {
		if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(?, 1, ?, ?, 'x')`, m.id, m.folder, m.uid); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	ops := []struct {
		id, message int
		typ, status string
	}{
		{1, 1, OpMove, OpPending},
		{2, 2, OpExpunge, OpNeedsAttention},
		{3, 1, OpMove, OpDone},
		{4, 3, OpMove, OpPending},
	}
	for _, o := range ops {
		if _, err := st.DB.Exec(`INSERT INTO ops(id, account_id, type, message_id, status) VALUES(?, 1, ?, ?, ?)`,
			o.id, o.typ, o.message, o.status); err != nil {
			t.Fatalf("insert op: %v", err)
		}
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	for _, o := range ops {
		got, err := st.GetOp(int64(o.id))
		if err != nil {
			t.Fatalf("op %d missing after wipe: %v", o.id, err)
		}
		if got.Status != o.status {
			t.Fatalf("op %d status = %q after wipe, want %q (wipe must not silently cancel ops)", o.id, got.Status, o.status)
		}
	}
}
