package store

import (
	"path/filepath"
	"testing"
)

func opsTestStore(t *testing.T) *Store {
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
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES
		(10, 1, 1, 100, 'm10@x', 0),
		(11, 1, 1, 101, 'm11@x', 0)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	return st
}

func boolPtr(b bool) *bool { return &b }

func TestPendingFlagsOpSurvivesStaleSyncFlagWrite(t *testing.T) {
	st := opsTestStore(t)
	if _, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, ""); err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	flags, err := st.MessageFlags(10)
	if err != nil || flags&1 == 0 {
		t.Fatalf("mirror flags after enqueue = %d, err %v; want seen bit set", flags, err)
	}
	if _, err := st.UpdateMessageFlags(1, 100, 0, 0); err != nil {
		t.Fatalf("update flags: %v", err)
	}
	flags, _ = st.MessageFlags(10)
	if flags&1 == 0 {
		t.Fatal("stale sync flag write reverted a message with a pending flags op back to unread")
	}
	row := &MessageRow{UID: 100, MessageID: "m10@x", Flags: 0}
	if _, _, err := st.UpsertMessage(1, 1, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	flags, _ = st.MessageFlags(10)
	if flags&1 == 0 {
		t.Fatal("stale sync envelope upsert reverted a message with a pending flags op back to unread")
	}
}

func TestSyncFlagWriteAppliesWithoutPendingOp(t *testing.T) {
	st := opsTestStore(t)
	changed, err := st.UpdateMessageFlags(1, 101, 1, 0)
	if err != nil {
		t.Fatalf("update flags: %v", err)
	}
	if !changed {
		t.Fatal("sync flag write on a message without pending ops must apply")
	}
	flags, _ := st.MessageFlags(11)
	if flags&1 == 0 {
		t.Fatal("flags not applied")
	}
}

func TestReconcileDeletionSkipsRowWithPendingMove(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.EnqueueMove(1, 10, 2, ""); err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	route, err := st.MessageRoute(10)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.FolderID != 2 {
		t.Fatalf("mirror folder after enqueue = %d, want 2 (Archive)", route.FolderID)
	}
	if route.UID < 1<<31 {
		t.Fatalf("mirror uid after enqueue = %d, want a provisional uid in the reserved range", route.UID)
	}
	if err := st.DeleteMessage(2, route.UID); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if _, err := st.MessageRoute(10); err != nil {
		t.Fatal("reconcile deleted a row that a pending move op still targets")
	}
}

func TestReconcileDeletionAppliesWithoutPendingOp(t *testing.T) {
	st := opsTestStore(t)
	if err := st.DeleteMessage(1, 101); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}
	if _, err := st.MessageRoute(11); err == nil {
		t.Fatal("row without pending ops must be deleted by reconcile")
	}
}
