package sync

import (
	"path/filepath"
	"sort"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

type fillConn struct {
	msgs      map[uint32]mail.Msg
	condStore bool
}

func (c *fillConn) Close()            {}
func (c *fillConn) Capabilities() []string {
	return nil
}
func (c *fillConn) HasCap(cap imap.Cap) bool {
	return cap == imap.CapCondStore && c.condStore
}
func (c *fillConn) ListFolders() ([]mail.FolderInfo, error) { return nil, nil }
func (c *fillConn) Select(name string) (*mail.SelectData, error) {
	var max uint32
	for uid := range c.msgs {
		if uid > max {
			max = uid
		}
	}
	return &mail.SelectData{UIDValidity: 1, UIDNext: max + 1, NumMessages: uint32(len(c.msgs))}, nil
}
func (c *fillConn) SelectRW(name string) (*mail.SelectData, error) { return c.Select(name) }
func (c *fillConn) FetchEnvelopes(uids []uint32) ([]mail.Msg, error) {
	var out []mail.Msg
	for _, uid := range uids {
		if msg, ok := c.msgs[uid]; ok {
			out = append(out, msg)
		}
	}
	return out, nil
}
func (c *fillConn) FetchFlags(changedSince uint64) ([]mail.Msg, error) {
	var out []mail.Msg
	for _, msg := range c.msgs {
		if msg.ModSeq > changedSince {
			out = append(out, msg)
		}
	}
	return out, nil
}
func (c *fillConn) FetchUIDs() ([]uint32, error) {
	out := make([]uint32, 0, len(c.msgs))
	for uid := range c.msgs {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
func (c *fillConn) FetchRaw(uid uint32) ([]byte, error) { return nil, nil }
func (c *fillConn) FetchThreadHeaders(uids []uint32) (map[uint32]mail.ThreadHeaders, error) {
	return nil, nil
}
func (c *fillConn) StoreFlag(uid uint32, flag imap.Flag, add bool) error    { return nil }
func (c *fillConn) StoreFlags(uids []uint32, flag imap.Flag, add bool) error { return nil }
func (c *fillConn) MoveMessage(uid uint32, dest string) (uint32, error)     { return 0, nil }
func (c *fillConn) DeleteMessage(uid uint32) error                          { return nil }
func (c *fillConn) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	return 0, nil
}

func fillMsg(uid uint32, mid string, modSeq uint64) mail.Msg {
	return mail.Msg{
		UID:       uid,
		MessageID: mid,
		Subject:   "subject " + mid,
		From:      []mail.Addr{{Email: "sender@x.test"}},
		To:        []mail.Addr{{Email: "a@x.test"}},
		Date:      time.Unix(1700000000+int64(uid)*60, 0),
		ModSeq:    modSeq,
	}
}

func newRefillFixture(t *testing.T, condStore bool) (*Manager, *store.Store, *fillConn, mail.Account, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	folderID, err := st.UpsertFolder(1, "INBOX", "inbox")
	if err != nil {
		t.Fatalf("upsert folder: %v", err)
	}
	cl := &fillConn{condStore: condStore, msgs: map[uint32]mail.Msg{
		1: fillMsg(1, "m1@x", 10),
		2: fillMsg(2, "m2@x", 11),
		3: fillMsg(3, "m3@x", 12),
	}}
	acct := mail.Account{ID: 1, Kind: "imap", Email: "a@x.test"}
	return NewManager(st), st, cl, acct, folderID
}

func rowCount(t *testing.T, st *store.Store, folderID int64) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE folder_id = ?", folderID).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func runRefillPhases(t *testing.T, condStore bool) {
	t.Helper()
	m, st, cl, acct, folderID := newRefillFixture(t, condStore)
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if n := rowCount(t, st, folderID); n != 3 {
		t.Fatalf("round 1: local rows = %d, want 3", n)
	}
	if _, err := st.DB.Exec("DELETE FROM messages WHERE folder_id = ? AND uid = 2", folderID); err != nil {
		t.Fatalf("delete local row: %v", err)
	}
	if _, err := st.DB.Exec("DELETE FROM first_seen WHERE account_id = 1 AND msg_key = 'mid:m2@x'"); err != nil {
		t.Fatalf("delete first_seen: %v", err)
	}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if n := rowCount(t, st, folderID); n != 3 {
		t.Fatalf("round 2: local rows = %d, want 3: a message the server still has, below the uid watermark, must be refilled, not lost forever", n)
	}
	var seenAt int64
	if err := st.DB.QueryRow("SELECT seen_at FROM first_seen WHERE account_id = 1 AND msg_key = 'mid:m2@x'").Scan(&seenAt); err != nil {
		t.Fatalf("first_seen for refilled message: %v", err)
	}
	if want := int64(1700000000 + 2*60); seenAt != want {
		t.Fatalf("refilled message seen_at = %d, want its date %d: a refill is not a new arrival and must not look like one", seenAt, want)
	}
	if _, err := st.DB.Exec("DELETE FROM messages WHERE folder_id = ? AND uid = 3", folderID); err != nil {
		t.Fatalf("delete local row uid 3: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO ops(account_id, type, message_id, prior_folder_id, prior_uid, status)
		VALUES(1, 'move', 0, ?, 3, 'pending')`, folderID); err != nil {
		t.Fatalf("insert active op: %v", err)
	}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 3: %v", err)
	}
	if n := rowCount(t, st, folderID); n != 2 {
		t.Fatalf("round 3: local rows = %d, want 2: a uid claimed by an in-flight op's prior must not be refilled underneath it", n)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'done'"); err != nil {
		t.Fatalf("settle op: %v", err)
	}
	if _, err := m.syncFolder(cl, acct, folderID, "INBOX"); err != nil {
		t.Fatalf("round 4: %v", err)
	}
	if n := rowCount(t, st, folderID); n != 3 {
		t.Fatalf("round 4: local rows = %d, want 3: once the op settles, the hole must be refilled", n)
	}
}

func TestSyncFolderRefillsBelowWatermarkCondStore(t *testing.T) {
	runRefillPhases(t, true)
}

func TestSyncFolderRefillsBelowWatermarkPlain(t *testing.T) {
	runRefillPhases(t, false)
}
