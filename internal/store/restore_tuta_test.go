package store

import (
	"path/filepath"
	"testing"
)

func tutaRestoreStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, kind, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'tuta', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0),
		      (2, 'b', 'b@x.test', 'imap', 'h', 993, 'tls', 'h', 465, 'tls', 'b@x.test', '', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (3, 1, 'Trash', 'trash'), (4, 1, 'Drafts', 'drafts'),
		(11, 2, 'INBOX', 'inbox'), (13, 2, 'Trash', 'trash'), (14, 2, 'Drafts', 'drafts')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags, trash_origin_folder_id) VALUES
		(10, 1, 3, 101, 'borrowed@x', 16, 1),
		(11, 1, 3, 102, 'real-draft@x', 16, 0),
		(20, 2, 13, 101, 'imap-draft@x', 16, 11)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	return st
}

func TestResolveRestoreFolderTutaOriginBeatsDerivedDraftBit(t *testing.T) {
	st := tutaRestoreStore(t)
	dest, err := st.ResolveRestoreFolder(1, 10)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 1 {
		t.Fatalf("tuta restore dest = %d, want 1 (inbox, the recorded origin); the folder-derived draft bit must not hijack it to drafts", dest)
	}
}

func TestResolveRestoreFolderTutaDraftWithoutOriginStillDrafts(t *testing.T) {
	st := tutaRestoreStore(t)
	dest, err := st.ResolveRestoreFolder(1, 11)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 4 {
		t.Fatalf("tuta restore dest = %d, want 4 (drafts) when no origin is recorded", dest)
	}
}

func TestResolveRestoreFolderImapDraftBitStillFirst(t *testing.T) {
	st := tutaRestoreStore(t)
	dest, err := st.ResolveRestoreFolder(2, 20)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dest != 14 {
		t.Fatalf("imap restore dest = %d, want 14 (drafts); the intrinsic draft bit keeps priority for imap", dest)
	}
}

