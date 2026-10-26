package store

import (
	"path/filepath"
	"testing"
	"time"
)

func restoreTestStore(t *testing.T) *Store {
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
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash'),
		(4, 1, 'Drafts', 'drafts'), (5, 1, 'Junk', 'junk')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES
		(10, 1, 2, 100, 'arch@x', 0),
		(11, 1, 3, 101, 'trashed-draft@x', 16),
		(12, 1, 3, 102, 'trashed-plain@x', 0),
		(13, 1, 1, 103, 'inbox@x', 0)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	return st
}

func trashOrigin(t *testing.T, st *Store, messageID int64) int64 {
	t.Helper()
	var origin int64
	if err := st.DB.QueryRow("SELECT trash_origin_folder_id FROM messages WHERE id = ?", messageID).Scan(&origin); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	return origin
}

func TestDeleteRecordsTrashOriginAndRestoreResolvesIt(t *testing.T) {
	st := restoreTestStore(t)
	if _, err := st.EnqueueMove(1, 10, 3, ""); err != nil {
		t.Fatalf("delete to trash: %v", err)
	}
	if got := trashOrigin(t, st, 10); got != 2 {
		t.Fatalf("origin after delete from archive = %d, want 2 (archive)", got)
	}
	dest, err := st.ResolveRestoreFolder(1, 10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 2 {
		t.Fatalf("restore dest = %d, want 2 (archive, its recorded origin)", dest)
	}
	if _, err := st.EnqueueMove(1, 10, dest, ""); err != nil {
		t.Fatalf("restore move: %v", err)
	}
	if got := trashOrigin(t, st, 10); got != 0 {
		t.Fatalf("origin after leaving trash = %d, want 0 (cleared)", got)
	}
}

func TestRestorePrefersIntrinsicDraftMarkOverOrigin(t *testing.T) {
	st := restoreTestStore(t)
	if _, err := st.DB.Exec("UPDATE messages SET trash_origin_folder_id = 1 WHERE id = 11"); err != nil {
		t.Fatalf("seed origin: %v", err)
	}
	dest, err := st.ResolveRestoreFolder(1, 11)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 4 {
		t.Fatalf("restore dest for draft-flagged message = %d, want 4 (drafts)", dest)
	}
}

func TestRestoreFallsBackToInboxWithoutOrigin(t *testing.T) {
	st := restoreTestStore(t)
	dest, err := st.ResolveRestoreFolder(1, 12)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 1 {
		t.Fatalf("restore dest without origin = %d, want 1 (inbox)", dest)
	}
}

func TestRestoreFallsBackToInboxWhenOriginFolderGone(t *testing.T) {
	st := restoreTestStore(t)
	if _, err := st.EnqueueMove(1, 10, 3, ""); err != nil {
		t.Fatalf("delete to trash: %v", err)
	}
	if _, err := st.DB.Exec("DELETE FROM folders WHERE id = 2"); err != nil {
		t.Fatalf("drop origin folder: %v", err)
	}
	dest, err := st.ResolveRestoreFolder(1, 10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 1 {
		t.Fatalf("restore dest with origin folder gone = %d, want 1 (inbox)", dest)
	}
}

func TestTrashOriginSurvivesReopenAndOpPurge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES
		(10, 1, 2, 100, 'arch@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	opID, err := st.EnqueueMove(1, 10, 3, "")
	if err != nil {
		t.Fatalf("delete to trash: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'done', created_at = created_at - 7200 WHERE id = ?", opID); err != nil {
		t.Fatalf("mark op done: %v", err)
	}
	st.DB.Close()
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.DB.Close()
	if err := st2.PurgeTerminalOps(time.Now().Unix()); err != nil {
		t.Fatalf("purge ops: %v", err)
	}
	dest, err := st2.ResolveRestoreFolder(1, 10)
	if err != nil {
		t.Fatalf("resolve after reopen and purge: %v", err)
	}
	if dest != 2 {
		t.Fatalf("restore dest after reopen and op purge = %d, want 2 (archive)", dest)
	}
}

func TestRedeleteFromAnotherFolderRewritesOrigin(t *testing.T) {
	st := restoreTestStore(t)
	if _, err := st.EnqueueMove(1, 13, 3, ""); err != nil {
		t.Fatalf("delete from inbox: %v", err)
	}
	if got := trashOrigin(t, st, 13); got != 1 {
		t.Fatalf("origin after delete from inbox = %d, want 1", got)
	}
	if _, err := st.EnqueueMove(1, 13, 1, ""); err != nil {
		t.Fatalf("restore to inbox: %v", err)
	}
	if _, err := st.EnqueueMove(1, 13, 2, ""); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := st.EnqueueMove(1, 13, 3, ""); err != nil {
		t.Fatalf("delete from archive: %v", err)
	}
	if got := trashOrigin(t, st, 13); got != 2 {
		t.Fatalf("origin after re-delete from archive = %d, want 2", got)
	}
	dest, err := st.ResolveRestoreFolder(1, 13)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 2 {
		t.Fatalf("restore dest after re-delete = %d, want 2 (archive)", dest)
	}
}
