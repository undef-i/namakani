package store

import (
	"path/filepath"
	"testing"
)

func openGmailTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, kind, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'g', 'g@x.test', 'gmail', '', 0, 'none', '', 0, 'none', '', '', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Work', ''), (3, 1, 'All Mail', 'all')`); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGmailUIDAssignmentIsStablePerFolder(t *testing.T) {
	st := openGmailTestStore(t)
	uid, err := st.AssignGmailUID(1, "INBOX", "msgA")
	if err != nil || uid != 1 {
		t.Fatalf("first assign = %d, %v; want 1", uid, err)
	}
	again, err := st.AssignGmailUID(1, "INBOX", "msgA")
	if err != nil || again != 1 {
		t.Fatalf("re-assign = %d, %v; want stable 1", again, err)
	}
	other, err := st.AssignGmailUID(1, "Work", "msgA")
	if err != nil || other != 1 {
		t.Fatalf("same message in another folder = %d, %v; want its own uid 1", other, err)
	}
	second, err := st.AssignGmailUID(1, "INBOX", "msgB")
	if err != nil || second != 2 {
		t.Fatalf("second message = %d, %v; want 2", second, err)
	}
	id, ok, err := st.GmailIDForUID(1, "INBOX", 2)
	if err != nil || !ok || id != "msgB" {
		t.Fatalf("GmailIDForUID = %q, %v, %v; want msgB", id, ok, err)
	}
	m, max, err := st.GmailUIDMap(1, "INBOX")
	if err != nil || max != 2 || m["msgA"] != 1 || m["msgB"] != 2 {
		t.Fatalf("GmailUIDMap = %v max %d, %v", m, max, err)
	}
}

func TestGmailIdentityIsSingleAcrossFolderRows(t *testing.T) {
	st := openGmailTestStore(t)
	for _, f := range []struct {
		folderID int64
		name     string
	}{
		{1, "INBOX"}, {2, "Work"}, {3, "All Mail"},
	} {
		uid, err := st.AssignGmailUID(1, f.name, "msgA")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, flags)
			VALUES(1, ?, ?, 'rfc-a@x', 0)`, f.folderID, uid); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.DB.Query("SELECT id, folder_id FROM messages ORDER BY folder_id")
	if err != nil {
		t.Fatal(err)
	}
	var rowIDs []int64
	var folderOf = map[int64]int64{}
	for rows.Next() {
		var id, fid int64
		if err := rows.Scan(&id, &fid); err != nil {
			t.Fatal(err)
		}
		rowIDs = append(rowIDs, id)
		folderOf[id] = fid
	}
	rows.Close()
	for _, id := range rowIDs {
		identity, err := st.IdentityForRow(1, id)
		if err != nil {
			t.Fatal(err)
		}
		if identity != "g:msgA" {
			t.Fatalf("row %d identity = %q; want the single g:msgA", id, identity)
		}
	}
	target, ok, err := st.ResolveIdentityRow(1, "g:msgA", 2, 0)
	if err != nil || !ok {
		t.Fatalf("resolve with anchor folder: ok=%v err=%v", ok, err)
	}
	if folderOf[target.RowID] != 2 {
		t.Fatalf("resolved row is in folder %d; want the anchored folder 2 row", folderOf[target.RowID])
	}
	cands, err := candidateRows(st.DB, 1, "g:msgA")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidate rows = %d; want all 3 folder rows of the message", len(cands))
	}
}

func TestGmailHistoryIDRoundTrip(t *testing.T) {
	st := openGmailTestStore(t)
	hid, err := st.GmailHistoryID(1)
	if err != nil || hid != "" {
		t.Fatalf("unset history id = %q, %v; want empty", hid, err)
	}
	if err := st.SetGmailHistoryID(1, "12345"); err != nil {
		t.Fatal(err)
	}
	hid, err = st.GmailHistoryID(1)
	if err != nil || hid != "12345" {
		t.Fatalf("history id = %q, %v; want 12345", hid, err)
	}
	if err := st.SetGmailHistoryID(1, "67890"); err != nil {
		t.Fatal(err)
	}
	hid, _ = st.GmailHistoryID(1)
	if hid != "67890" {
		t.Fatalf("history id after update = %q; want 67890", hid)
	}
}

func TestGmailFolderSnapshotJoinsFlags(t *testing.T) {
	st := openGmailTestStore(t)
	uidA, _ := st.AssignGmailUID(1, "INBOX", "msgA")
	uidB, _ := st.AssignGmailUID(1, "INBOX", "msgB")
	if _, err := st.DB.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, flags)
		VALUES(1, 1, ?, 'rfc-a@x', 3)`, uidA); err != nil {
		t.Fatal(err)
	}
	snap, err := st.GmailFolderSnapshot(1, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot rows = %d; want 2", len(snap))
	}
	byID := map[string]GmailFolderRow{}
	for _, r := range snap {
		byID[r.GmailID] = r
	}
	if r := byID["msgA"]; r.UID != uidA || r.Flags != 3 || !r.HasRow {
		t.Fatalf("msgA snapshot = %+v; want uid %d flags 3 with row", r, uidA)
	}
	if r := byID["msgB"]; r.UID != uidB || r.HasRow {
		t.Fatalf("msgB snapshot = %+v; want uid %d without row", r, uidB)
	}
}
