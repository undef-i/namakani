package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func midlessRow() *MessageRow {
	return &MessageRow{
		UID:       21,
		MessageID: "",
		Subject:   "测试",
		FromJSON:  `[{"name":"","email":"a@x.test"}]`,
		ToJSON:    `[{"name":"","email":"b@tuta.test"}]`,
		CcJSON:    "[]",
		DateTS:    1761000000,
		Size:      4321,
	}
}

func TestFirstSeenMidlessIdentitySurvivesRebuild(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, _ := seedFirstSeenFolders(t, s)

	id1, inserted, err := s.UpsertMessage(1, inboxID, midlessRow())
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
	if strings.HasPrefix(got[0].Key, "row:") {
		t.Fatalf("mid-less identity keyed on the volatile row id: %q", got[0].Key)
	}
	if got[0].Special != "inbox" || got[0].RowID != id1 {
		t.Fatalf("first_seen = %+v, want special inbox row %d", got[0], id1)
	}
	key := got[0].Key

	if _, err := s.DB.Exec(`DELETE FROM messages WHERE id = ?`, id1); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	id2, inserted, err := s.UpsertMessage(1, inboxID, midlessRow())
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if !inserted || id2 == id1 {
		t.Fatalf("rebuild: inserted=%v id2=%d id1=%d, want a genuinely new row", inserted, id2, id1)
	}
	got = firstSeenRows(t, s)
	if len(got) != 1 {
		t.Fatalf("after rebuild first_seen rows = %d, want exactly 1 (at most one arrival identity per message)", len(got))
	}
	if got[0].Key != key || got[0].RowID != id1 {
		t.Fatalf("identity moved across rebuild: %+v, want key %q anchored at row %d", got[0], key, id1)
	}
}

func TestFirstSeenMidlessFirstFolderWinsAcrossRebuild(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, junkID := seedFirstSeenFolders(t, s)

	id1, _, err := s.UpsertMessage(1, junkID, midlessRow())
	if err != nil {
		t.Fatalf("upsert junk: %v", err)
	}
	if _, err := s.DB.Exec(`DELETE FROM messages WHERE id = ?`, id1); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if _, _, err := s.UpsertMessage(1, inboxID, midlessRow()); err != nil {
		t.Fatalf("upsert inbox: %v", err)
	}
	got := firstSeenRows(t, s)
	if len(got) != 1 {
		t.Fatalf("first_seen rows = %d, want 1", len(got))
	}
	if got[0].Special != "junk" {
		t.Fatalf("first folder = %q, want junk: a later incarnation in inbox must not reclassify the arrival", got[0].Special)
	}
}

func TestFirstSeenMidlessDistinctMessagesDistinctKeys(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.DB.Close()
	inboxID, _, _ := seedFirstSeenFolders(t, s)

	if _, _, err := s.UpsertMessage(1, inboxID, midlessRow()); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	other := midlessRow()
	other.UID = 22
	other.Size = 9999
	if _, _, err := s.UpsertMessage(1, inboxID, other); err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	got := firstSeenRows(t, s)
	if len(got) != 2 {
		t.Fatalf("first_seen rows = %d, want 2 distinct identities", len(got))
	}
	if got[0].Key == got[1].Key {
		t.Fatalf("distinct messages share identity key %q", got[0].Key)
	}
}
