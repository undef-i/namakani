package api

import (
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/auth"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

type tgSendCounter struct{ n atomic.Int64 }

func (c *tgSendCounter) count() int64 { return c.n.Load() }

func stubTGCall(t *testing.T) *tgSendCounter {
	t.Helper()
	c := &tgSendCounter{}
	orig := tgCall
	tgCall = func(token, method string, payload any) (json.RawMessage, error) {
		if method == "sendRichMessage" {
			c.n.Add(1)
		}
		return json.RawMessage(`{"message_id":1}`), nil
	}
	t.Cleanup(func() { tgCall = orig })
	return c
}

func openTGNotifyStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func newTGNotifyServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	serverKey := make([]byte, 32)
	for i := range serverKey {
		serverKey[i] = byte(i + 1)
	}
	enc, err := auth.Encrypt(serverKey, tgTestToken)
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if err := st.SetSetting(settingTGTokenEnc, enc); err != nil {
		t.Fatalf("store token: %v", err)
	}
	if err := st.SetSetting(settingTGChatID, "424242"); err != nil {
		t.Fatalf("store chat id: %v", err)
	}
	if err := st.SetSetting(settingTGEnabled, "1"); err != nil {
		t.Fatalf("store enabled: %v", err)
	}
	return &Server{
		st:        st,
		serverKey: serverKey,
		keys:      make(map[string][]byte),
		tgSeen:    make(map[string]tgChat),
	}
}

func seedTGNotifyData(t *testing.T, st *store.Store) int64 {
	t.Helper()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	id, inserted, err := st.UpsertMessage(1, 1, &store.MessageRow{
		UID: 100, MessageID: "m10@x", Subject: "hello",
		FromJSON: "[]", ToJSON: "[]", CcJSON: "[]", DateTS: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if !inserted {
		t.Fatalf("seed message inserted = false, want true")
	}
	return id
}

func waitTGCount(t *testing.T, c *tgSendCounter, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("telegram sends = %d, want %d", c.count(), want)
}

func TestTGNotifySameChangeOnlyPushesOnce(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	id := seedTGNotifyData(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)

	ch := syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}}
	s.tgNotify(ch)
	waitTGCount(t, sends, 1)

	s.tgNotify(ch)
	time.Sleep(300 * time.Millisecond)
	if got := sends.count(); got != 1 {
		t.Fatalf("same change delivered twice: sends = %d, want 1", got)
	}
}

func TestTGNotifyMoveOutAndBackDoesNotRepush(t *testing.T) {
	st := openTGNotifyStore(t, filepath.Join(t.TempDir(), "namakani.db"))
	defer st.DB.Close()
	id := seedTGNotifyData(t, st)
	s := newTGNotifyServer(t, st)
	sends := stubTGCall(t)

	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}})
	waitTGCount(t, sends, 1)

	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = ?`, id); err != nil {
		t.Fatalf("move out: %v", err)
	}
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 2, Inserted: []int64{id}})

	if _, err := st.DB.Exec(`UPDATE messages SET folder_id = 1 WHERE id = ?`, id); err != nil {
		t.Fatalf("move back: %v", err)
	}
	s.tgNotify(syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}})

	time.Sleep(300 * time.Millisecond)
	if got := sends.count(); got != 1 {
		t.Fatalf("move out and back: sends = %d, want 1", got)
	}
}

func TestTGNotifyRestartDoesNotRepush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namakani.db")
	sends := stubTGCall(t)

	st1 := openTGNotifyStore(t, path)
	id := seedTGNotifyData(t, st1)
	ch := syncmgr.Change{AccountID: 1, FolderID: 1, Inserted: []int64{id}, Initial: true}
	s1 := newTGNotifyServer(t, st1)
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
	time.Sleep(300 * time.Millisecond)
	if got := sends.count(); got != 0 {
		t.Fatalf("after restart: sends = %d, want 0", got)
	}
}
