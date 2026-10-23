package sync

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

func e2eCount(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestSyncFolderCondStoreDeletionReconcile(t *testing.T) {
	addr := os.Getenv("WM_E2E_IMAP_ADDR")
	if addr == "" {
		t.Skip("WM_E2E_IMAP_ADDR not set")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	acct := mail.Account{
		ID:       1,
		Kind:     "imap",
		Email:    os.Getenv("WM_E2E_USER"),
		ImapHost: host,
		ImapPort: port,
		Username: os.Getenv("WM_E2E_USER"),
		Password: os.Getenv("WM_E2E_PASS"),
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'e2e', ?, ?, ?, '', '', 0, '', ?, '', 0)`, acct.Email, host, port, acct.Username); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	cl, err := mail.Connect(acct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cl.Close()
	if !cl.HasCap(imap.CapCondStore) {
		t.Skip("server does not advertise CONDSTORE")
	}
	m := NewManager(st)
	folderID, err := st.UpsertFolder(acct.ID, "INBOX", "inbox")
	if err != nil {
		t.Fatalf("upsert folder: %v", err)
	}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("sync round 1: %v", err)
	}
	state, err := st.FolderState(folderID)
	if err != nil {
		t.Fatalf("folder state: %v", err)
	}
	if state.HighestModSeq == 0 {
		t.Fatal("server returned no modseqs in round 1; incremental path unreachable")
	}
	sel, err := cl.Select("INBOX")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	total := e2eCount(t, st, "SELECT COUNT(*) FROM messages WHERE folder_id = ?", folderID)
	if int(sel.NumMessages) != total {
		t.Fatalf("round 1: local %d messages, remote %d", total, sel.NumMessages)
	}
	if total < 5 {
		t.Fatalf("round 1: expected the seeded mailbox, got %d messages", total)
	}
	uids, err := cl.FetchUIDs()
	if err != nil {
		t.Fatalf("fetch uids: %v", err)
	}
	msgs, err := cl.FetchEnvelopes(uids)
	if err != nil {
		t.Fatalf("fetch envelopes: %v", err)
	}
	uidByMsgID := make(map[string]uint32)
	for _, msg := range msgs {
		uidByMsgID[msg.MessageID] = msg.UID
	}
	targets := map[string]string{
		"p1-2@localhost": "invoice",
		"p1-3@localhost": "hiking",
	}
	localIDs := make(map[string]int64)
	for mid := range targets {
		uid, ok := uidByMsgID[mid]
		if !ok {
			t.Fatalf("seeded message %s not found upstream", mid)
		}
		id, ok := st.LocalID(folderID, uid)
		if !ok {
			t.Fatalf("seeded message %s not stored locally", mid)
		}
		localIDs[mid] = id
	}
	controlID, ok := st.LocalID(folderID, uidByMsgID["p2-html@localhost"])
	if !ok {
		t.Fatal("control message p2-html not stored locally")
	}
	m.prewarm(context.Background(), cl, acct)
	var invoicePath string
	atts, err := st.ListAttachments(localIDs["p1-2@localhost"])
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	for _, a := range atts {
		if a.Filename == "invoice.txt" {
			invoicePath = a.LocalPath
		}
	}
	if invoicePath == "" {
		t.Fatal("invoice.txt attachment was not cached to disk by prewarm")
	}
	if _, err := os.Stat(invoicePath); err != nil {
		t.Fatalf("cached attachment missing on disk: %v", err)
	}
	var controlPath string
	controlAtts, err := st.ListAttachments(controlID)
	if err != nil {
		t.Fatalf("list control attachments: %v", err)
	}
	for _, a := range controlAtts {
		if a.LocalPath != "" {
			controlPath = a.LocalPath
		}
	}
	if controlPath == "" {
		t.Fatal("control message attachments were not cached by prewarm")
	}
	for mid, word := range targets {
		if n := e2eCount(t, st, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", word); n == 0 {
			t.Fatalf("fts has no hit for %q before deletion (%s)", word, mid)
		}
	}
	cl2, err := mail.Connect(acct, nil)
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	if _, err := cl2.SelectRW("INBOX"); err != nil {
		t.Fatalf("select rw: %v", err)
	}
	for mid := range targets {
		if err := cl2.DeleteMessage(uidByMsgID[mid]); err != nil {
			t.Fatalf("upstream delete %s: %v", mid, err)
		}
	}
	cl2.Close()
	change, err := m.syncFolder(cl, acct, folderID, "INBOX")
	if err != nil {
		t.Fatalf("sync round 2: %v", err)
	}
	removed := make(map[int64]bool)
	for _, id := range change.Removed {
		removed[id] = true
	}
	for mid, id := range localIDs {
		if !removed[id] {
			t.Errorf("change.Removed missing %s (local id %d)", mid, id)
		}
		if _, ok := st.LocalID(folderID, uidByMsgID[mid]); ok {
			t.Errorf("message %s still stored locally after upstream deletion", mid)
		}
	}
	if n := e2eCount(t, st, "SELECT COUNT(*) FROM messages WHERE folder_id = ?", folderID); n != total-2 {
		t.Errorf("local count after deletion = %d, want %d", n, total-2)
	}
	for mid, word := range targets {
		if n := e2eCount(t, st, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?", word); n != 0 {
			t.Errorf("fts still has %d hits for %q after deletion of %s", n, word, mid)
		}
		if n := e2eCount(t, st, "SELECT COUNT(*) FROM attachments WHERE message_id = ?", localIDs[mid]); n != 0 {
			t.Errorf("attachment rows remain for deleted %s", mid)
		}
		if n := e2eCount(t, st, "SELECT COUNT(*) FROM bodies WHERE message_id = ?", localIDs[mid]); n != 0 {
			t.Errorf("body row remains for deleted %s", mid)
		}
	}
	if _, err := os.Stat(invoicePath); !os.IsNotExist(err) {
		t.Errorf("cached attachment file still on disk after deletion: %s", invoicePath)
	}
	if _, ok := st.LocalID(folderID, uidByMsgID["p2-html@localhost"]); !ok {
		t.Error("control message vanished")
	}
	if _, err := os.Stat(controlPath); err != nil {
		t.Errorf("control attachment file lost: %v", err)
	}
	change3, err := m.syncFolder(cl, acct, folderID, "INBOX")
	if err != nil {
		t.Fatalf("sync round 3: %v", err)
	}
	if len(change3.Removed) != 0 {
		t.Errorf("round 3 removed %v, want none", change3.Removed)
	}
	if n := e2eCount(t, st, "SELECT COUNT(*) FROM messages WHERE folder_id = ?", folderID); n != total-2 {
		t.Errorf("local count after round 3 = %d, want %d", n, total-2)
	}
}
