package sync

import (
	"path/filepath"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestCondStoreWatermarkIgnoresEnvelopeModSeqs(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	folderID, err := st.UpsertFolder(1, "INBOX", "inbox")
	if err != nil {
		t.Fatalf("upsert folder: %v", err)
	}
	cl := &fillConn{condStore: true, msgs: map[uint32]mail.Msg{
		10: {UID: 10, MessageID: "w10@x", Subject: "old", From: []mail.Addr{{Email: "s@x.test"}}, Date: time.Unix(1700000000, 0), ModSeq: 50},
	}}
	acct := mail.Account{ID: 1, Kind: "imap", Email: "a@x.test"}
	m := NewManager(st)
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	changed := cl.msgs[10]
	changed.Flags = mail.FlagSeen
	changed.ModSeq = 99
	cl.msgs[10] = changed
	cl.msgs[11] = mail.Msg{UID: 11, MessageID: "w11@x", Subject: "new", From: []mail.Addr{{Email: "s@x.test"}}, Date: time.Unix(1700001000, 0), ModSeq: 100}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	localFlags := func() uint32 {
		t.Helper()
		id, ok := st.LocalID(folderID, 10)
		if !ok {
			t.Fatal("uid 10 missing locally")
		}
		var flags uint32
		if err := st.DB.QueryRow("SELECT flags FROM messages WHERE id = ?", id).Scan(&flags); err != nil {
			t.Fatalf("read flags: %v", err)
		}
		return flags
	}
	if got := localFlags(); got != mail.FlagSeen {
		t.Fatalf("after round 2, uid 10 flags = %d, want seen (%d): a flag change at modseq 99 must not be skipped just because a newer message was created at modseq 100 in the same pass", got, mail.FlagSeen)
	}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if got := localFlags(); got != mail.FlagSeen {
		t.Fatalf("after round 3, uid 10 flags = %d, want seen (%d)", got, mail.FlagSeen)
	}
	state, err := st.FolderState(folderID)
	if err != nil {
		t.Fatalf("folder state: %v", err)
	}
	if state.HighestModSeq < 99 {
		t.Fatalf("persisted watermark = %d, want at least 99 once the change has been applied", state.HighestModSeq)
	}
}
