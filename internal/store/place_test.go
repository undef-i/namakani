package store

import (
	"path/filepath"
	"testing"
)

func seedPlaceStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name) VALUES(1, 1, 'Inbox'), (2, 1, 'Archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	return st
}

func TestPlacementVersionTracksOnlyPlacements(t *testing.T) {
	st := seedPlaceStore(t)
	if v, err := st.ListVersion(); err != nil || v != 0 {
		t.Fatalf("initial version = %d, %v, want 0", v, err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(1, 1, 1, 11, 'm1@x')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if v, err := st.ListVersion(); err != nil || v != 1 {
		t.Fatalf("version after insert = %d, %v, want 1", v, err)
	}
	pvs, err := st.PlacementVersions([]int64{1})
	if err != nil || pvs[1] != 1 {
		t.Fatalf("placement of inserted row = %v, %v, want id 1 at version 1", pvs, err)
	}
	if _, err := st.DB.Exec(`UPDATE messages SET flags = 1, snippet = 'x' WHERE id = 1`); err != nil {
		t.Fatalf("update flags: %v", err)
	}
	if v, err := st.ListVersion(); err != nil || v != 1 {
		t.Fatalf("version after flags update = %d, %v, want 1 (flags are not placements)", v, err)
	}
	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = 1`); err != nil {
		t.Fatalf("move message: %v", err)
	}
	if v, err := st.ListVersion(); err != nil || v != 2 {
		t.Fatalf("version after move = %d, %v, want 2", v, err)
	}
	pvs, err = st.PlacementVersions([]int64{1})
	if err != nil || pvs[1] != 2 {
		t.Fatalf("placement after move = %v, %v, want id 1 at version 2", pvs, err)
	}
	if _, err := st.DB.Exec(`DELETE FROM messages WHERE id = 1`); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	if v, err := st.ListVersion(); err != nil || v != 3 {
		t.Fatalf("version after delete = %d, %v, want 3", v, err)
	}
	pvs, err = st.PlacementVersions([]int64{1})
	if err != nil || pvs[1] != 3 {
		t.Fatalf("placement after delete = %v, %v, want tombstone at version 3", pvs, err)
	}
}

func TestPlacementPruneKeepsLatestPerMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name) VALUES(1, 1, 'Inbox'), (2, 1, 'Archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(1, 1, 1, 11, 'm1@x'), (2, 1, 1, 12, 'm2@x')`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = 1`); err != nil {
		t.Fatalf("move: %v", err)
	}
	st.DB.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.DB.Close()
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM place_log`).Scan(&n); err != nil {
		t.Fatalf("count place log: %v", err)
	}
	if n != 2 {
		t.Fatalf("place log rows after prune = %d, want 2 (latest per message)", n)
	}
	if v, err := st.ListVersion(); err != nil || v != 3 {
		t.Fatalf("version after prune = %d, %v, want 3 (prune must not rewind the version)", v, err)
	}
	pvs, err := st.PlacementVersions([]int64{1, 2})
	if err != nil || pvs[1] != 3 || pvs[2] != 2 {
		t.Fatalf("placements after prune = %v, %v, want {1:3, 2:2}", pvs, err)
	}
}
