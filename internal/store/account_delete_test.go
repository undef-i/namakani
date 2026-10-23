package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedAccount(t *testing.T, s *Store, label string, bodyText string) (accountID, folderID, messageID, attachmentID int64) {
	t.Helper()
	res, err := s.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(?, ?, 'imap.example.com', 993, 'tls', 'smtp.example.com', 465, 'tls', ?, 'enc', 1)`,
		label, label+"@example.com", label)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	accountID, _ = res.LastInsertId()
	res, err = s.DB.Exec("INSERT INTO folders(account_id, name, special_use) VALUES(?, 'INBOX', 'inbox')", accountID)
	if err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	folderID, _ = res.LastInsertId()
	res, err = s.DB.Exec("INSERT INTO messages(account_id, folder_id, uid, subject) VALUES(?, ?, 1, 'hello')",
		accountID, folderID)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	messageID, _ = res.LastInsertId()
	if _, err := s.DB.Exec("INSERT INTO bodies(message_id, text_body) VALUES(?, ?)", messageID, bodyText); err != nil {
		t.Fatalf("insert body: %v", err)
	}
	res, err = s.DB.Exec("INSERT INTO attachments(message_id, part_path, filename, size) VALUES(?, '1', 'a.bin', 4)", messageID)
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	attachmentID, _ = res.LastInsertId()
	path := s.AttachmentFilePath(attachmentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir attachments: %v", err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}
	if err := s.SetAttachmentLocalPath(attachmentID, path); err != nil {
		t.Fatalf("set local path: %v", err)
	}
	if _, err := s.DB.Exec(
		"INSERT INTO messages_fts(rowid, subject, senders, recipients, body) VALUES(?, 'hello', 'a@b.c', 'd@e.f', 'hello')",
		messageID,
	); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	return accountID, folderID, messageID, attachmentID
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func dbFileSize(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	if _, err := s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	return fi.Size()
}

func TestDeleteAccountRemovesRowsFilesAndShrinksDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "namakani.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.DB.Close()

	big := strings.Repeat("x", 1<<20)
	doomed, _, doomedMsg, doomedAtt := seedAccount(t, s, "doomed", big)
	keeper, _, keeperMsg, keeperAtt := seedAccount(t, s, "keeper", "small body")
	keeperFile := s.AttachmentFilePath(keeperAtt)
	doomedFile := s.AttachmentFilePath(doomedAtt)

	before := dbFileSize(t, s, dbPath)

	if err := s.DeleteAccount(doomed); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	if n := countRows(t, s, "SELECT COUNT(*) FROM accounts WHERE id = ?", doomed); n != 0 {
		t.Fatalf("account row still present (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM folders WHERE account_id = ?", doomed); n != 0 {
		t.Fatalf("folder rows still present (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM messages WHERE account_id = ?", doomed); n != 0 {
		t.Fatalf("message rows still present (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM bodies WHERE message_id = ?", doomedMsg); n != 0 {
		t.Fatalf("body row still present (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM attachments WHERE id = ?", doomedAtt); n != 0 {
		t.Fatalf("attachment row still present (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM messages_fts WHERE rowid = ?", doomedMsg); n != 0 {
		t.Fatalf("fts row still present (%d)", n)
	}
	if _, err := os.Stat(doomedFile); !os.IsNotExist(err) {
		t.Fatalf("attachment file still present (err = %v)", err)
	}

	if n := countRows(t, s, "SELECT COUNT(*) FROM accounts WHERE id = ?", keeper); n != 1 {
		t.Fatalf("keeper account missing (%d)", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM messages WHERE id = ?", keeperMsg); n != 1 {
		t.Fatalf("keeper message missing (%d)", n)
	}
	if _, err := os.Stat(keeperFile); err != nil {
		t.Fatalf("keeper attachment file gone: %v", err)
	}

	after := dbFileSize(t, s, dbPath)
	if after >= before {
		t.Fatalf("db did not shrink after delete + VACUUM: before = %d, after = %d", before, after)
	}
}
