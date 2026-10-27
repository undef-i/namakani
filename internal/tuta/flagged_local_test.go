package tuta

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestTutaFlaggedStaysLocal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@tuta.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@tuta.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var calls atomic.Int64
	s := newSession()
	s.st = st
	s.acct = mail.Account{ID: 1}
	s.folders = []tutaFolder{{name: "Inbox", entriesID: "entriesInbox", setListID: "setsList1", setID: "setInbox"}}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "/mailsetentry/"):
			return jsonResponse(r, `[
				{"1452":["entriesInbox","entryElem1"],"1456":[["mailList1","mailElem1"]]},
				{"1452":["entriesInbox","entryElem2"],"1456":[["mailList1","mailElem2"]]}
			]`), nil
		case strings.Contains(r.URL.Path, "/mail/"):
			return jsonResponse(r, `[
				{"99":["mailList1","mailElem1"],"107":"1700000000000","110":"1"},
				{"99":["mailList1","mailElem2"],"107":"1700000001000"}
			]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	if _, err := s.Select("Inbox"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	uid1 := s.cur.uidByID["mailElem1"]
	uid2 := s.cur.uidByID["mailElem2"]
	if uid1 == 0 || uid2 == 0 {
		t.Fatalf("uids = %d/%d, want both assigned", uid1, uid2)
	}

	envs, err := s.FetchEnvelopes([]uint32{uid1, uid2})
	if err != nil {
		t.Fatalf("FetchEnvelopes: %v", err)
	}
	for _, e := range envs {
		if e.Flags&mail.FlagFlagged != 0 {
			t.Fatalf("server pinned attribute leaked into local flags for uid %d", e.UID)
		}
	}

	calls.Store(0)
	if err := s.StoreFlags([]uint32{uid1}, imap.FlagFlagged, true); err != nil {
		t.Fatalf("StoreFlags(FlagFlagged): %v", err)
	}
	if err := s.StoreFlag(uid2, imap.FlagFlagged, true); err != nil {
		t.Fatalf("StoreFlag(FlagFlagged): %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("flagged store made %d server calls, want 0 (flagged is local-only for tuta)", n)
	}

	folderID, err := st.UpsertFolder(1, "Inbox", "inbox")
	if err != nil {
		t.Fatalf("UpsertFolder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, flags) VALUES(1, ?, ?, 'mailElem1@tuta', ?)`,
		folderID, uid1, mail.FlagFlagged); err != nil {
		t.Fatalf("insert starred row: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(account_id, folder_id, uid, message_id, flags) VALUES(1, ?, ?, 'mailElem2@tuta', 0)`,
		folderID, uid2); err != nil {
		t.Fatalf("insert unstarred row: %v", err)
	}
	msgs, err := s.FetchFlags(0)
	if err != nil {
		t.Fatalf("FetchFlags: %v", err)
	}
	byUID := make(map[uint32]uint32, len(msgs))
	for _, m := range msgs {
		byUID[m.UID] = m.Flags
	}
	if byUID[uid1]&mail.FlagFlagged == 0 {
		t.Fatalf("locally starred uid %d lost its flag in FetchFlags (flags=%d)", uid1, byUID[uid1])
	}
	if byUID[uid2]&mail.FlagFlagged != 0 {
		t.Fatalf("unstarred uid %d gained a flag in FetchFlags (flags=%d)", uid2, byUID[uid2])
	}
}
