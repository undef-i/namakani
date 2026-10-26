package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"

	"github.com/emersion/go-imap/v2"
)

type fakeCall struct {
	Method string
	Folder string
	UIDs   []uint32
	Flag   imap.Flag
	Add    bool
	Dest   string
}

type fakeProvider struct {
	mu              sync.Mutex
	folders         map[string]map[uint32]uint32
	nextUID         map[string]uint32
	calls           []fakeCall
	missingErr      bool
	storeFlagsDelay time.Duration
	failStoreFlags  []error
	failSelectOnce  map[string]int
	failMove        []error
	failDelete      []error
	moveNoUID       bool
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{
		folders: map[string]map[uint32]uint32{},
		nextUID: map[string]uint32{},
	}
}

func (p *fakeProvider) seed(folder string, uid, flags uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.folders[folder] == nil {
		p.folders[folder] = map[uint32]uint32{}
	}
	p.folders[folder][uid] = flags
	if uid >= p.nextUID[folder] {
		p.nextUID[folder] = uid + 1
	}
}

func (p *fakeProvider) flags(folder string, uid uint32) (uint32, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.folders[folder]
	if !ok {
		return 0, false
	}
	f, ok := m[uid]
	return f, ok
}

func (p *fakeProvider) callsOf(method string) []fakeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []fakeCall
	for _, c := range p.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (p *fakeProvider) conn() mail.Conn {
	return &fakeConn{p: p}
}

type fakeConn struct {
	p        *fakeProvider
	selected string
}

func (c *fakeConn) Close()                             {}
func (c *fakeConn) Capabilities() []string             { return nil }
func (c *fakeConn) HasCap(cap imap.Cap) bool           { return false }
func (c *fakeConn) ListFolders() ([]mail.FolderInfo, error) {
	return nil, nil
}
func (c *fakeConn) Select(name string) (*mail.SelectData, error) {
	c.selected = name
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if c.p.failSelectOnce[name] > 0 {
		c.p.failSelectOnce[name]--
		return nil, fmt.Errorf("select %s failed", name)
	}
	next := c.p.nextUID[name]
	if next == 0 {
		next = 1
	}
	return &mail.SelectData{NumMessages: uint32(len(c.p.folders[name])), UIDNext: next}, nil
}

func (c *fakeConn) FetchUIDs() ([]uint32, error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	out := make([]uint32, 0, len(c.p.folders[c.selected]))
	for uid := range c.p.folders[c.selected] {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
func (c *fakeConn) SelectRW(name string) (*mail.SelectData, error) { return c.Select(name) }
func (c *fakeConn) FetchEnvelopes(uids []uint32) ([]mail.Msg, error) {
	return nil, nil
}
func (c *fakeConn) FetchFlags(uint64) ([]mail.Msg, error) { return nil, nil }
func (c *fakeConn) FetchRaw(uid uint32) ([]byte, error)   { return nil, nil }
func (c *fakeConn) FetchThreadHeaders(uids []uint32) (map[uint32]mail.ThreadHeaders, error) {
	return nil, nil
}

func popErr(q *[]error) error {
	if len(*q) == 0 {
		return nil
	}
	err := (*q)[0]
	*q = (*q)[1:]
	return err
}

func (c *fakeConn) StoreFlag(uid uint32, flag imap.Flag, add bool) error {
	return c.StoreFlags([]uint32{uid}, flag, add)
}

func (c *fakeConn) StoreFlags(uids []uint32, flag imap.Flag, add bool) error {
	p := c.p
	if p.storeFlagsDelay > 0 {
		time.Sleep(p.storeFlagsDelay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fakeCall{Method: "StoreFlags", Folder: c.selected, UIDs: append([]uint32{}, uids...), Flag: flag, Add: add})
	if err := popErr(&p.failStoreFlags); err != nil {
		return err
	}
	bit := uint32(0)
	switch flag {
	case imap.FlagSeen:
		bit = mail.FlagSeen
	case imap.FlagFlagged:
		bit = mail.FlagFlagged
	}
	for _, uid := range uids {
		cur, ok := p.folders[c.selected][uid]
		if !ok {
			if p.missingErr {
				return fmt.Errorf("message not found in %s", c.selected)
			}
			continue
		}
		if add {
			cur |= bit
		} else {
			cur &^= bit
		}
		p.folders[c.selected][uid] = cur
	}
	return nil
}

func (c *fakeConn) MoveMessage(uid uint32, dest string) (uint32, error) {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fakeCall{Method: "MoveMessage", Folder: c.selected, UIDs: []uint32{uid}, Dest: dest})
	if err := popErr(&p.failMove); err != nil {
		return 0, err
	}
	flags, ok := p.folders[c.selected][uid]
	if !ok {
		return 0, fmt.Errorf("message not found in %s", c.selected)
	}
	delete(p.folders[c.selected], uid)
	if p.folders[dest] == nil {
		p.folders[dest] = map[uint32]uint32{}
	}
	newUID := p.nextUID[dest]
	if newUID == 0 {
		newUID = 1
	}
	p.nextUID[dest] = newUID + 1
	p.folders[dest][newUID] = flags
	if p.moveNoUID {
		p.folders[dest][newUID+1000] = 0
		return 0, nil
	}
	return newUID, nil
}

func (c *fakeConn) DeleteMessage(uid uint32) error {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fakeCall{Method: "DeleteMessage", Folder: c.selected, UIDs: []uint32{uid}})
	if err := popErr(&p.failDelete); err != nil {
		return err
	}
	if _, ok := p.folders[c.selected][uid]; !ok {
		if p.missingErr {
			return fmt.Errorf("message not found in %s", c.selected)
		}
		return nil
	}
	delete(p.folders[c.selected], uid)
	return nil
}

func (c *fakeConn) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fakeCall{Method: "AppendMessage", Folder: folder})
	if p.folders[folder] == nil {
		p.folders[folder] = map[uint32]uint32{}
	}
	uid := p.nextUID[folder]
	if uid == 0 {
		uid = 1
	}
	p.nextUID[folder] = uid + 1
	var mask uint32
	for _, f := range flags {
		switch f {
		case imap.FlagSeen:
			mask |= mail.FlagSeen
		case imap.FlagDraft:
			mask |= mail.FlagDraft
		}
	}
	p.folders[folder][uid] = mask
	return uid, nil
}

type drainFixture struct {
	srv          *Server
	st           *store.Store
	p            *fakeProvider
	sessionToken string
}

func newDrainFixture(t *testing.T) *drainFixture {
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
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash'), (4, 1, 'Drafts', 'drafts')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	p := newFakeProvider()
	srv := New(st, nil, syncmgr.NewManager(st))
	srv.drainOpen = func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error) {
		return p.conn(), nil
	}
	srv.appendOpen = func(key []byte, accountID int64) (mail.Conn, error) {
		return p.conn(), nil
	}
	return &drainFixture{srv: srv, st: st, p: p}
}

func (f *drainFixture) addMessage(t *testing.T, id int64, folderID int64, folderName string, uid, flags uint32) {
	t.Helper()
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(?, 1, ?, ?, ?, ?)`, id, folderID, uid, fmt.Sprintf("m%d@x", id), flags); err != nil {
		t.Fatalf("insert message %d: %v", id, err)
	}
	f.p.seed(folderName, uid, flags)
}

func (f *drainFixture) postFlags(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/messages/%d/flags", id), strings.NewReader(body))
	req.SetPathValue("id", fmt.Sprint(id))
	rec := httptest.NewRecorder()
	f.srv.handleSetFlags(rec, req)
	return rec
}

func TestSetFlagsHandlerReturnsBeforeProviderWork(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.storeFlagsDelay = 3 * time.Second
	start := time.Now()
	rec := f.postFlags(t, 10, `{"seen":true}`)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("flags handler took %v; it must return after the local enqueue, not after provider work", elapsed)
	}
	var resp struct {
		Flags uint32 `json:"flags"`
		OpID  int64  `json:"op_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Flags&1 == 0 {
		t.Fatalf("response flags = %d, want seen applied", resp.Flags)
	}
	if resp.OpID == 0 {
		t.Fatal("response carries no op_id")
	}
	if n := len(f.p.callsOf("StoreFlags")); n != 0 {
		t.Fatalf("provider was called %d times during the request; want 0", n)
	}
	mirror, _ := f.st.MessageFlags(10)
	if mirror&1 == 0 {
		t.Fatal("mirror flags were not updated by the enqueue")
	}
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(resp.OpID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status after drain = %q, want done", o.Status)
	}
	pf, ok := f.p.flags("INBOX", 100)
	if !ok || pf&1 == 0 {
		t.Fatalf("provider flags after drain = %d (ok=%v), want seen set", pf, ok)
	}
	if n := len(f.p.callsOf("StoreFlags")); n != 1 {
		t.Fatalf("provider StoreFlags calls = %d, want exactly 1", n)
	}
}

func TestBatchFlagsOneProviderCallPerFolder(t *testing.T) {
	f := newDrainFixture(t)
	ids := []int64{}
	for i := int64(0); i < 30; i++ {
		id := 100 + i
		f.addMessage(t, id, 1, "INBOX", uint32(1000+i), 0)
		ids = append(ids, id)
	}
	body, _ := json.Marshal(map[string]any{"ids": ids, "seen": true})
	req := httptest.NewRequest(http.MethodPost, "/api/messages/flags", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	f.srv.handleBatchFlags(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []struct {
			ID   int64  `json:"id"`
			OpID int64  `json:"op_id"`
		} `json:"items"`
		Failed []struct {
			ID int64 `json:"id"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 30 || len(resp.Failed) != 0 {
		t.Fatalf("batch result: %d items, %d failed; want 30/0", len(resp.Items), len(resp.Failed))
	}
	for _, id := range ids {
		fl, _ := f.st.MessageFlags(id)
		if fl&1 == 0 {
			t.Fatalf("message %d not marked in the mirror by the batch enqueue", id)
		}
	}
	f.srv.drainAccount(nil, 1)
	calls := f.p.callsOf("StoreFlags")
	if len(calls) != 1 {
		t.Fatalf("provider StoreFlags calls = %d, want 1 batched call for the whole folder", len(calls))
	}
	if len(calls[0].UIDs) != 30 {
		t.Fatalf("batched call carried %d uids, want 30", len(calls[0].UIDs))
	}
	for i := int64(0); i < 30; i++ {
		pf, ok := f.p.flags("INBOX", uint32(1000+i))
		if !ok || pf&1 == 0 {
			t.Fatalf("provider message uid %d not marked seen after drain", 1000+i)
		}
	}
}

func TestDrainFlagsTransientFailureRetriesThenSucceeds(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.failStoreFlags = []error{errors.New("dial tcp: i/o timeout"), errors.New("dial tcp: i/o timeout")}
	opID, _, err := f.st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for round := 1; round <= 2; round++ {
		f.srv.drainAccount(nil, 1)
		o, _ := f.st.GetOp(opID)
		if o.Status != store.OpPending {
			t.Fatalf("after failed round %d op status = %q, want pending (scheduled retry)", round, o.Status)
		}
		if o.Attempts != round {
			t.Fatalf("after failed round %d attempts = %d", round, o.Attempts)
		}
		if _, err := f.st.DB.Exec("UPDATE ops SET next_attempt = 0 WHERE id = ?", opID); err != nil {
			t.Fatalf("reset backoff: %v", err)
		}
	}
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after recovery = %q, want done", o.Status)
	}
	pf, _ := f.p.flags("INBOX", 100)
	if pf&1 == 0 {
		t.Fatal("provider never received the flag after retries")
	}
}

func TestDrainFlagsAuthFailureParksWithoutRollback(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.failStoreFlags = []error{&imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "AUTHENTICATIONFAILED"}}
	opID, _, err := f.st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("op status after auth failure = %q, want needs_attention on the first attempt", o.Status)
	}
	mirror, _ := f.st.MessageFlags(10)
	if mirror&1 == 0 {
		t.Fatal("parking the op rolled back the user's visible state; it must stay until the user decides")
	}
}

func TestDrainFlagsMessageGoneCompletesAndDropsRow(t *testing.T) {
	f := newDrainFixture(t)
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	f.p.missingErr = true
	opID, _, err := f.st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q, want done (the provider no longer has the message)", o.Status)
	}
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("local row for a provider-gone message must be dropped once its op resolves")
	}
}

func boolPtr(b bool) *bool { return &b }
