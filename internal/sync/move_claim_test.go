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

type moveConn struct {
	folders map[string]map[uint32]mail.Msg
	cur     string
}

func (c *moveConn) Close()            {}
func (c *moveConn) Capabilities() []string {
	return nil
}
func (c *moveConn) HasCap(cap imap.Cap) bool { return false }
func (c *moveConn) ListFolders() ([]mail.FolderInfo, error) {
	return nil, nil
}
func (c *moveConn) Select(name string) (*mail.SelectData, error) {
	c.cur = name
	var max uint32
	for uid := range c.folders[name] {
		if uid > max {
			max = uid
		}
	}
	return &mail.SelectData{UIDValidity: 1, UIDNext: max + 1, NumMessages: uint32(len(c.folders[name]))}, nil
}
func (c *moveConn) SelectRW(name string) (*mail.SelectData, error) { return c.Select(name) }
func (c *moveConn) FetchEnvelopes(uids []uint32) ([]mail.Msg, error) {
	var out []mail.Msg
	for _, uid := range uids {
		if msg, ok := c.folders[c.cur][uid]; ok {
			out = append(out, msg)
		}
	}
	return out, nil
}
func (c *moveConn) FetchFlags(changedSince uint64) ([]mail.Msg, error) {
	var out []mail.Msg
	for _, msg := range c.folders[c.cur] {
		out = append(out, msg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}
func (c *moveConn) FetchUIDs() ([]uint32, error) {
	out := make([]uint32, 0, len(c.folders[c.cur]))
	for uid := range c.folders[c.cur] {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
func (c *moveConn) FetchRaw(uid uint32) ([]byte, error) { return nil, nil }
func (c *moveConn) FetchThreadHeaders(uids []uint32) (map[uint32]mail.ThreadHeaders, error) {
	return nil, nil
}
func (c *moveConn) StoreFlag(uid uint32, flag imap.Flag, add bool) error    { return nil }
func (c *moveConn) StoreFlags(uids []uint32, flag imap.Flag, add bool) error { return nil }
func (c *moveConn) MoveMessage(uid uint32, dest string) (uint32, error)     { return 0, nil }
func (c *moveConn) DeleteMessage(uid uint32) error                          { return nil }
func (c *moveConn) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	return 0, nil
}

func moveMsg(uid uint32, mid string) mail.Msg {
	return mail.Msg{
		UID:       uid,
		MessageID: mid,
		Subject:   "subject " + mid,
		From:      []mail.Addr{{Email: "sender@x.test"}},
		To:        []mail.Addr{{Email: "a@x.test"}},
		Date:      time.Unix(1700000000, 0),
	}
}

type moveFixture struct {
	m         *Manager
	st        *store.Store
	cl        *moveConn
	acct      mail.Account
	inboxID   int64
	archiveID int64
	rowID     int64
}

func newMoveFixture(t *testing.T) *moveFixture {
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
	inboxID, err := st.UpsertFolder(1, "INBOX", "inbox")
	if err != nil {
		t.Fatalf("upsert inbox: %v", err)
	}
	archiveID, err := st.UpsertFolder(1, "Archive", "archive")
	if err != nil {
		t.Fatalf("upsert archive: %v", err)
	}
	cl := &moveConn{folders: map[string]map[uint32]mail.Msg{
		"INBOX":   {100: moveMsg(100, "mv@x")},
		"Archive": {},
	}}
	f := &moveFixture{m: NewManager(st), st: st, cl: cl, acct: mail.Account{ID: 1, Kind: "imap", Email: "a@x.test"}, inboxID: inboxID, archiveID: archiveID}
	if _, err := f.m.syncFolder(cl, f.acct, inboxID, "INBOX"); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	id, ok := st.LocalID(inboxID, 100)
	if !ok {
		t.Fatal("initial sync did not create the inbox row")
	}
	f.rowID = id
	if err := st.SaveBody(id, "cached body text", "", "cached body text", false); err != nil {
		t.Fatalf("save body: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE messages SET thread_key = 'thread-preserved' WHERE id = ?", id); err != nil {
		t.Fatalf("set thread key: %v", err)
	}
	return f
}

func (f *moveFixture) externalMove() {
	f.cl.folders["INBOX"] = map[uint32]mail.Msg{}
	f.cl.folders["Archive"] = map[uint32]mail.Msg{55: moveMsg(55, "mv@x")}
}

func (f *moveFixture) assertClaimed(t *testing.T) {
	t.Helper()
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE account_id = 1 AND message_id = 'mv@x'").Scan(&n); err != nil {
		t.Fatalf("count identity rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("identity rows = %d, want exactly 1 (no delete+reinsert duplicate)", n)
	}
	var folderID int64
	var uid uint32
	var threadKey string
	if err := f.st.DB.QueryRow("SELECT folder_id, uid, thread_key FROM messages WHERE id = ?", f.rowID).Scan(&folderID, &uid, &threadKey); err != nil {
		t.Fatalf("original row %d must survive the move: %v", f.rowID, err)
	}
	if folderID != f.archiveID || uid != 55 {
		t.Fatalf("row now at folder %d uid %d, want archive %d uid 55", folderID, uid, f.archiveID)
	}
	if threadKey != "thread-preserved" {
		t.Fatalf("thread_key = %q, want preserved", threadKey)
	}
	text, _, ok, err := f.st.GetBody(f.rowID)
	if err != nil || !ok || text != "cached body text" {
		t.Fatalf("body after move: ok=%v text=%q err=%v, want the cached body", ok, text, err)
	}
}

func TestExternalMoveSourceSyncsFirst(t *testing.T) {
	f := newMoveFixture(t)
	f.externalMove()
	if _, err := f.m.syncFolder(f.cl, f.acct, f.inboxID, "INBOX"); err != nil {
		t.Fatalf("sync inbox after move: %v", err)
	}
	if _, ok := f.st.LocalID(f.inboxID, 100); !ok {
		t.Fatal("row deleted by the first source reconcile instead of being held for the target folder")
	}
	if _, err := f.m.syncFolder(f.cl, f.acct, f.archiveID, "Archive"); err != nil {
		t.Fatalf("sync archive after move: %v", err)
	}
	f.assertClaimed(t)
}

func TestExternalMoveTargetSyncsFirst(t *testing.T) {
	f := newMoveFixture(t)
	f.externalMove()
	if _, err := f.m.syncFolder(f.cl, f.acct, f.archiveID, "Archive"); err != nil {
		t.Fatalf("sync archive after move: %v", err)
	}
	if _, err := f.m.syncFolder(f.cl, f.acct, f.inboxID, "INBOX"); err != nil {
		t.Fatalf("sync inbox after move: %v", err)
	}
	f.assertClaimed(t)
}

func TestExternalDeleteStillDeletesAfterGrace(t *testing.T) {
	f := newMoveFixture(t)
	f.cl.folders["INBOX"] = map[uint32]mail.Msg{}
	if _, err := f.m.syncFolder(f.cl, f.acct, f.inboxID, "INBOX"); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if _, err := f.m.syncFolder(f.cl, f.acct, f.inboxID, "INBOX"); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if _, ok := f.st.LocalID(f.inboxID, 100); ok {
		t.Fatal("row still present after two reconciles with no twin anywhere; a real deletion must land")
	}
	if _, _, ok, err := f.st.GetBody(f.rowID); err != nil || ok {
		t.Fatalf("body after real deletion: ok=%v err=%v, want gone", ok, err)
	}
}
