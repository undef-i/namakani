package tuta

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestListFolderEnumeratesMailsFromEntryTuples(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@tuta.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@tuta.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	s := newSession()
	s.st = st
	s.acct = mail.Account{ID: 1}
	s.folders = []tutaFolder{{name: "Inbox", entriesID: "entriesInbox"}}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailsetentry/"):
			return jsonResponse(r, `[{"1452":["entriesInbox","entryElem1"],"1456":[["mailList1","mailElem1"]]}]`), nil
		case strings.Contains(r.URL.Path, "/mail/"):
			return jsonResponse(r, `[{"99":["mailList1","mailElem1"],"107":"1700000000000"}]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	fc, err := s.listFolderLocked("Inbox")
	if err != nil {
		t.Fatalf("listFolderLocked: %v", err)
	}
	if fc.total != 1 || len(fc.ids) != 1 {
		t.Fatalf("enumerated ids=%d total=%d, want 1/1 (entry mail association is a list of one id tuple)", len(fc.ids), fc.total)
	}
	if _, ok := fc.meta["mailElem1"]; !ok {
		t.Fatalf("meta missing for mailElem1, got %v", fc.meta)
	}
}
