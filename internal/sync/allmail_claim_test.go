package sync

import (
	"path/filepath"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type allTwinFixture struct {
	m    *Manager
	st   *store.Store
	cl   *moveConn
	acct mail.Account
}

func newAllTwinFixture(t *testing.T, trashBody, allBody bool) *allTwinFixture {
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
		(2, 1, 'All Mail', 'all'), (3, 1, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES
		(10, 1, 3, 100, 'twin@x', 0),
		(11, 1, 2, 200, 'twin@x', 0)`); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
	if trashBody {
		if err := st.SaveBody(10, "trash body", "", "trash body", false); err != nil {
			t.Fatalf("save trash body: %v", err)
		}
	}
	if allBody {
		if err := st.SaveBody(11, "all body", "", "all body", false); err != nil {
			t.Fatalf("save all body: %v", err)
		}
	}
	cl := &moveConn{folders: map[string]map[uint32]mail.Msg{
		"Trash":    {},
		"All Mail": {200: moveMsg(200, "twin@x")},
	}}
	return &allTwinFixture{
		m:    NewManager(st),
		st:   st,
		cl:   cl,
		acct: mail.Account{ID: 1, Kind: "proton", Email: "a@x.test"},
	}
}

func (f *allTwinFixture) route(t *testing.T, id int64) store.MessageRoute {
	t.Helper()
	r, err := f.st.MessageRoute(id)
	if err != nil {
		t.Fatalf("message row %d is gone: %v", id, err)
	}
	return r
}

func TestAllMailMirrorCannotDeleteTrashRowOnOnePass(t *testing.T) {
	f := newAllTwinFixture(t, false, true)
	if _, err := f.m.syncFolder(f.cl, f.acct, 3, "Trash"); err != nil {
		t.Fatalf("sync trash pass 1: %v", err)
	}
	if r := f.route(t, 10); r.FolderID != 3 || r.UID != 100 {
		t.Fatalf("after one absent listing the trash row sits in folder %d uid %d, want Trash (3) uid 100: a single listing must not re-file or kill it", r.FolderID, r.UID)
	}
	f.route(t, 11)
	if _, err := f.m.syncFolder(f.cl, f.acct, 3, "Trash"); err != nil {
		t.Fatalf("sync trash pass 2: %v", err)
	}
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("trash row survived two confirmed absent listings; a real deletion must still land")
	}
	if r := f.route(t, 11); r.FolderID != 2 || r.UID != 200 {
		t.Fatalf("all mail mirror ended in folder %d uid %d, want All Mail (2) uid 200: the mirror is not the claim's loser", r.FolderID, r.UID)
	}
}

func TestAllMailMirrorCannotRefileTrashRow(t *testing.T) {
	f := newAllTwinFixture(t, true, false)
	if _, err := f.m.syncFolder(f.cl, f.acct, 3, "Trash"); err != nil {
		t.Fatalf("sync trash: %v", err)
	}
	if r := f.route(t, 10); r.FolderID != 3 || r.UID != 100 {
		t.Fatalf("trash row was re-filed to folder %d uid %d, want Trash (3) uid 100: presence in All Mail is not a move into All Mail", r.FolderID, r.UID)
	}
	if r := f.route(t, 11); r.FolderID != 2 || r.UID != 200 {
		t.Fatalf("all mail mirror ended in folder %d uid %d, want All Mail (2) uid 200", r.FolderID, r.UID)
	}
}

func TestAllMailSyncCannotClaimPendingTrashRow(t *testing.T) {
	f := newAllTwinFixture(t, false, true)
	if err := f.st.SetPendingDelete(10, true); err != nil {
		t.Fatalf("mark trash row pending: %v", err)
	}
	if _, err := f.m.syncFolder(f.cl, f.acct, 2, "All Mail"); err != nil {
		t.Fatalf("sync all mail: %v", err)
	}
	if r := f.route(t, 10); r.FolderID != 3 || r.UID != 100 {
		t.Fatalf("pending trash row ended in folder %d uid %d after an All Mail sync, want Trash (3) uid 100: the mirror folder's sync must not claim it", r.FolderID, r.UID)
	}
	f.route(t, 11)
}
