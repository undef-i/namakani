package api

import (
	"path/filepath"
	"testing"
	"time"

	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

func seedArrivalFolders(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Junk', 'junk')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
}

func arrive(t *testing.T, st *store.Store, folderID int64, uid uint32, mid string, dateTS int64) int64 {
	t.Helper()
	id, inserted, err := st.UpsertMessage(1, folderID, &store.MessageRow{
		UID: uid, MessageID: mid, Subject: "hi " + mid,
		FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: dateTS,
	})
	if err != nil {
		t.Fatalf("arrive upsert: %v", err)
	}
	if !inserted {
		t.Fatalf("arrive upsert inserted = false, want true")
	}
	return id
}

func expectNoTGSends(t *testing.T, c *tgSendCounter, want int64) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if got := c.count(); got != want {
		t.Fatalf("telegram sends = %d, want %d", got, want)
	}
}

func TestTGArrivalJunkThenMovedToInboxNeverPushes(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	seedArrivalFolders(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)
	now := time.Now().Unix()

	junkRow := arrive(t, st, 3, 501, "<s1@x>", now)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 3, Inserted: []int64{junkRow}})
	expectNoTGSends(t, sends, 0)

	inboxRow := arrive(t, st, 1, 502, "<s1@x>", now)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{inboxRow}})
	expectNoTGSends(t, sends, 0)
}

func TestTGArrivalInboxPushesOnceAcrossArchiveAndBack(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	seedArrivalFolders(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)
	now := time.Now().Unix()

	id := arrive(t, st, 1, 601, "<s2@x>", now)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}})
	waitTGCount(t, sends, 1)

	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = ?`, id); err != nil {
		t.Fatalf("archive move: %v", err)
	}
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 2, Inserted: []int64{id}})
	expectNoTGSends(t, sends, 1)

	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("undo move back: %v", err)
	}
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}})
	expectNoTGSends(t, sends, 1)
}

func TestTGArrivalArchiveFirstThenInboxCopyDoesNotPush(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	seedArrivalFolders(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)
	now := time.Now().Unix()

	archRow := arrive(t, st, 2, 701, "<s3@x>", now)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 2, Inserted: []int64{archRow}})
	expectNoTGSends(t, sends, 0)

	inboxRow := arrive(t, st, 1, 702, "<s3@x>", now)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{inboxRow}})
	expectNoTGSends(t, sends, 0)
}

func TestTGArrivalRestartInitialDoesNotRepush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namakani.db")
	sends := stubTGCall(t)
	now := time.Now().Unix()

	st1 := openTGNotifyStore(t, path)
	seedArrivalFolders(t, st1)
	id := arrive(t, st1, 1, 801, "<s4@x>", now)
	s1 := newTGNotifyServer(t, st1)
	ch := syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}, Initial: true}
	s1.tgNotify(ch)
	waitTGCount(t, sends, 1)
	if err := st1.DB.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st2 := openTGNotifyStore(t, path)
	defer st2.DB.Close()
	s2 := newTGNotifyServer(t, st2)
	sends.n.Store(0)
	s2.tgNotify(ch)
	expectNoTGSends(t, sends, 0)
}

func TestTGArrivalInitialOldDateJustArrivedPushes(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	seedArrivalFolders(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)

	old := time.Now().Add(-24 * time.Hour).Unix()
	id := arrive(t, st, 1, 901, "<s5@x>", old)
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}, Initial: true})
	waitTGCount(t, sends, 1)
}
