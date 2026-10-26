package store

import (
	"path/filepath"
	"testing"
)

func TestCompletionAndPayloadWritesRequireLiveStatus(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES (1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	opID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if err := st.CompleteOp(opID); err != nil {
		t.Fatalf("complete pending op: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != OpPending {
		t.Fatalf("CompleteOp flipped a pending (never claimed) op to %q; completion must require in_flight so a stale finisher cannot land over a live op", o.Status)
	}
}

func TestUpdateOpPayloadRefusesTerminalOp(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES (1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	opID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	before, _ := st.GetOp(opID)
	if _, err := st.DB.Exec("UPDATE ops SET status = 'done' WHERE id = ?", opID); err != nil {
		t.Fatalf("force done: %v", err)
	}
	applied, err := st.UpdateOpPayload(opID, `{"sent":false}`)
	if err != nil {
		t.Fatalf("update payload: %v", err)
	}
	if applied {
		t.Fatalf("UpdateOpPayload rewrote the payload of a terminal op; an edit racing the drain could resurrect a delivered send as unsent")
	}
	after, _ := st.GetOp(opID)
	if after.Payload != before.Payload {
		t.Fatalf("terminal op payload changed: %q -> %q", before.Payload, after.Payload)
	}
}
