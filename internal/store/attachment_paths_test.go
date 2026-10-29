package store

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func seedAttachmentStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'INBOX', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES(1, 1, 1, 1, 'm1@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return st
}

func attachmentPath(t *testing.T, st *Store, id int64) string {
	t.Helper()
	var p string
	if err := st.DB.QueryRow(`SELECT local_path FROM attachments WHERE id = ?`, id).Scan(&p); err != nil {
		t.Fatalf("read attachment %d: %v", id, err)
	}
	return p
}

func TestOpenRepairsStaleAttachmentPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "namakani.db")
	st := seedAttachmentStore(t, path)
	legacy := filepath.Join(dir, "legacy")
	stale := func(id int64) string { return filepath.Join(legacy, strconv.FormatInt(id, 10)+".bin") }
	rows := []struct {
		id    int64
		local string
	}{
		{1, stale(1)},                  // recorded file gone, canonical file exists
		{2, stale(2)},                  // recorded file gone, canonical file absent too
		{3, st.AttachmentFilePath(3)},  // already canonical
		{4, filepath.Join(dir, "elsewhere.bin")}, // recorded file alive elsewhere
	}
	for _, r := range rows {
		if _, err := st.DB.Exec(`INSERT INTO attachments(id, message_id, part_path, filename, mime, size, local_path)
			VALUES(?, 1, '1', 'f.bin', 'application/octet-stream', 3, ?)`, r.id, r.local); err != nil {
			t.Fatalf("insert attachment %d: %v", r.id, err)
		}
	}
	if err := os.MkdirAll(st.AttachmentsDir(), 0o700); err != nil {
		t.Fatalf("mkdir attachments: %v", err)
	}
	for _, id := range []int64{1, 3} {
		if err := os.WriteFile(st.AttachmentFilePath(id), []byte("bin"), 0o600); err != nil {
			t.Fatalf("write canonical file %d: %v", id, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "elsewhere.bin"), []byte("bin"), 0o600); err != nil {
		t.Fatalf("write elsewhere file: %v", err)
	}
	st.DB.Close()

	st = reopenStore(t, path)
	defer st.DB.Close()
	if got := attachmentPath(t, st, 1); got != st.AttachmentFilePath(1) {
		t.Fatalf("attachment 1 local_path = %q, want the canonical %q whose file exists", got, st.AttachmentFilePath(1))
	}
	if got := attachmentPath(t, st, 2); got != stale(2) {
		t.Fatalf("attachment 2 local_path = %q, want it left at %q because no file exists in the attachments dir", got, stale(2))
	}
	if got := attachmentPath(t, st, 3); got != st.AttachmentFilePath(3) {
		t.Fatalf("attachment 3 local_path = %q, want it unchanged", got)
	}
	if got := attachmentPath(t, st, 4); got != filepath.Join(dir, "elsewhere.bin") {
		t.Fatalf("attachment 4 local_path = %q, want it unchanged because the recorded file exists", got)
	}
}
