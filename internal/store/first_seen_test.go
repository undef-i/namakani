package store

import (
	"path/filepath"
	"testing"
)

func seedFirstSeenFolders(t *testing.T, s *Store) (inboxID, archiveID, junkID int64) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	for _, f := range []struct {
		id      int64
		name    string
		special string
	}{{1, "INBOX", "inbox"}, {2, "Archive", "archive"}, {3, "Junk", "junk"}} {
		if _, err := s.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(?, 1, ?, ?)`,
			f.id, f.name, f.special); err != nil {
			t.Fatalf("insert folder: %v", err)
		}
	}
	return 1, 2, 3
}

func firstSeenRows(t *testing.T, s *Store) []struct {
	Key     string
	Special string
	RowID   int64
} {
	t.Helper()
	rows, err := s.DB.Query(`SELECT msg_key, folder_special, message_id FROM first_seen ORDER BY msg_key`)
	if err != nil {
		t.Fatalf("query first_seen: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Key     string
		Special string
		RowID   int64
	}
	for rows.Next() {
		var r struct {
			Key     string
			Special string
			RowID   int64
		}
		if err := rows.Scan(&r.Key, &r.Special, &r.RowID); err != nil {
			t.Fatalf("scan first_seen: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestFirstSeenRecordedOnInsert(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, _ := seedFirstSeenFolders(t, s)
	_, inserted, err := s.UpsertMessage(1, inboxID, &MessageRow{
		UID: 11, MessageID: "<a@x>", Subject: "hi", FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: 100,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !inserted {
		t.Fatalf("inserted = false, want true")
	}
	got := firstSeenRows(t, s)
	if len(got) != 1 {
		t.Fatalf("first_seen rows = %d, want 1", len(got))
	}
}

func TestFirstSeenNotDuplicatedOnReUpsert(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, _ := seedFirstSeenFolders(t, s)
	row := &MessageRow{UID: 11, MessageID: "<a@x>", Subject: "hi", FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: 100}
	if _, _, err := s.UpsertMessage(1, inboxID, row); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	_, inserted, err := s.UpsertMessage(1, inboxID, row)
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if inserted {
		t.Fatalf("second upsert inserted = true, want false")
	}
	if got := firstSeenRows(t, s); len(got) != 1 {
		t.Fatalf("first_seen rows = %d, want 1", len(got))
	}
}

func TestFirstSeenCrossFolderSameMessageIDKeepsArrival(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, junkID := seedFirstSeenFolders(t, s)
	junkRowID, _, err := s.UpsertMessage(1, junkID, &MessageRow{
		UID: 21, MessageID: "<b@x>", Subject: "spam", FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: 100,
	})
	if err != nil {
		t.Fatalf("upsert junk: %v", err)
	}
	if _, _, err := s.UpsertMessage(1, inboxID, &MessageRow{
		UID: 22, MessageID: "<b@x>", Subject: "spam", FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: 100,
	}); err != nil {
		t.Fatalf("upsert inbox copy: %v", err)
	}
	got := firstSeenRows(t, s)
	if len(got) != 1 {
		t.Fatalf("first_seen rows = %d, want 1", len(got))
	}
	if got[0].Special != "junk" || got[0].RowID != junkRowID {
		t.Fatalf("first_seen = %+v, want arrival kept as junk row %d", got[0], junkRowID)
	}
}

func TestFirstSeenBackfillOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	inboxID, _, junkID := seedFirstSeenFolders(t, s)
	if _, err := s.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject, date_ts)
		VALUES(41, 1, ?, 41, '<old@x>', 'old junk', 50)`, junkID); err != nil {
		t.Fatalf("insert legacy junk row: %v", err)
	}
	if _, err := s.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject, date_ts)
		VALUES(42, 1, ?, 42, '<old2@x>', 'old inbox', 60)`, inboxID); err != nil {
		t.Fatalf("insert legacy inbox row: %v", err)
	}
	if err := s.DB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.DB.Close()
	got := firstSeenRows(t, s2)
	if len(got) != 2 {
		t.Fatalf("first_seen rows after backfill = %d, want 2", len(got))
	}
}

func TestFirstSeenLocalMoveCreatesNothing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, archiveID, _ := seedFirstSeenFolders(t, s)
	if _, err := s.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject, date_ts)
		VALUES(51, 1, ?, 51, '<mv@x>', 'move me', 70)`, archiveID); err != nil {
		t.Fatalf("insert row: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE messages SET folder_id = ?, uid = ? WHERE id = ?`, inboxID, 99, 51); err != nil {
		t.Fatalf("local move update: %v", err)
	}
	if got := firstSeenRows(t, s); len(got) != 0 {
		t.Fatalf("first_seen rows after local move = %d, want 0", len(got))
	}
}
