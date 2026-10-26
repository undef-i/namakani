package tuta

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type serviceCall struct {
	method string
	path   string
}

func newCacheTestSession(t *testing.T) (*Session, *[]serviceCall) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@tuta.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@tuta.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	calls := &[]serviceCall{}
	s := newSession()
	s.st = st
	s.acct = mail.Account{ID: 1}
	s.folders = []tutaFolder{
		{name: "Inbox", special: "inbox", entriesID: "entriesInbox", setListID: "setList", setID: "setInbox"},
		{name: "Archive", special: "archive", kind: "5", setListID: "setList", setID: "setArchive"},
	}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailsetentry/"):
			return jsonResponse(r, `[
				{"1452":["entriesInbox","entryElem1"],"1456":[["mailList1","mailElem1"]]},
				{"1452":["entriesInbox","entryElem2"],"1456":[["mailList1","mailElem2"]]}
			]`), nil
		case strings.Contains(r.URL.Path, "simplemovemailservice"), strings.Contains(r.URL.Path, "movemailservice"), strings.Contains(r.URL.Path, "mailservice"):
			*calls = append(*calls, serviceCall{method: r.Method, path: r.URL.Path})
			return jsonResponse(r, `{}`), nil
		case strings.Contains(r.URL.Path, "/mail/"):
			return jsonResponse(r, `[
				{"99":["mailList1","mailElem1"],"107":"1700000000000"},
				{"99":["mailList1","mailElem2"],"107":"1700000001000"}
			]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	return s, calls
}

func tutaUIDOf(t *testing.T, s *Session, id string) uint32 {
	t.Helper()
	if s.cur == nil {
		t.Fatalf("no folder selected")
	}
	uid, ok := s.cur.uidByID[id]
	if !ok {
		t.Fatalf("no uid for %s (map %v)", id, s.cur.uidByID)
	}
	return uid
}

func countCalls(calls []serviceCall, method, pathPart string) int {
	n := 0
	for _, c := range calls {
		if c.method == method && strings.Contains(c.path, pathPart) {
			n++
		}
	}
	return n
}

func TestSelectThenDeleteTwiceInSameFolder(t *testing.T) {
	s, calls := newCacheTestSession(t)
	sel, err := s.SelectRW("Inbox")
	if err != nil {
		t.Fatalf("SelectRW: %v", err)
	}
	if sel.NumMessages != 2 {
		t.Fatalf("NumMessages=%d, want 2", sel.NumMessages)
	}
	uid1 := tutaUIDOf(t, s, "mailElem1")
	uid2 := tutaUIDOf(t, s, "mailElem2")
	if err := s.DeleteMessage(uid1); err != nil {
		t.Fatalf("first DeleteMessage: %v", err)
	}
	if err := s.DeleteMessage(uid2); err != nil {
		t.Fatalf("second DeleteMessage in the same selection: %v", err)
	}
	if n := countCalls(*calls, http.MethodDelete, "mailservice"); n != 2 {
		t.Fatalf("mailservice DELETE calls=%d, want 2 (calls %+v)", n, *calls)
	}
}

func TestMoveThenDeleteInSourceFolder(t *testing.T) {
	s, calls := newCacheTestSession(t)
	if _, err := s.SelectRW("Inbox"); err != nil {
		t.Fatalf("SelectRW: %v", err)
	}
	uid1 := tutaUIDOf(t, s, "mailElem1")
	uid2 := tutaUIDOf(t, s, "mailElem2")
	destUID, err := s.MoveMessage(uid1, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage: %v", err)
	}
	if destUID == 0 {
		t.Fatalf("MoveMessage returned destUID 0")
	}
	if err := s.DeleteMessage(uid2); err != nil {
		t.Fatalf("DeleteMessage in source folder after move: %v", err)
	}
	if n := countCalls(*calls, http.MethodPost, "simplemovemailservice"); n != 1 {
		t.Fatalf("simplemovemailservice POST calls=%d, want 1 (calls %+v)", n, *calls)
	}
	if n := countCalls(*calls, http.MethodDelete, "mailservice"); n != 1 {
		t.Fatalf("mailservice DELETE calls=%d, want 1 (calls %+v)", n, *calls)
	}
}
