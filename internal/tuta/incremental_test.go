package tuta

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func TestListFoldersSkipsLabelSets(t *testing.T) {
	s := newSession()
	s.mailGroups = []string{"mailGroup1"}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailboxgrouproot/"):
			return jsonResponse(r, `{"695":"root1","698":["mailGroup1"],"699":["mailbox1"]}`), nil
		case strings.Contains(r.URL.Path, "/mailbox/"):
			return jsonResponse(r, `{"127":"mailbox1","443":[{"441":"refId","442":["setsList1"]}]}`), nil
		case strings.Contains(r.URL.Path, "/mailset/"):
			return jsonResponse(r, `[
				{"431":["setsList1","setInbox"],"433":"0","436":"1","1459":["entriesInbox"]},
				{"431":["setsList1","setLabel"],"433":"0","436":"8","1459":["entriesLabel"]},
				{"431":["setsList1","setSched"],"433":"0","436":"10","1459":["entriesSched"]}
			]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	folders, err := s.listFolders()
	if err != nil {
		t.Fatalf("listFolders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("folders = %d, want 2 (label set skipped)", len(folders))
	}
	if folders[1].name != "Scheduled" || folders[1].kind != "10" {
		t.Fatalf("kind 10 folder = %+v, want name Scheduled", folders[1])
	}
}

func TestTurnsSerialize(t *testing.T) {
	s := newSession()
	endUser := s.UserTurn()
	acquired := make(chan struct{})
	go func() {
		end, ok := s.BackgroundTurn(t.Context())
		if ok {
			close(acquired)
			end()
		}
	}()
	select {
	case <-acquired:
		t.Fatal("background turn acquired while user turn held")
	case <-time.After(150 * time.Millisecond):
	}
	endUser()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("background turn not acquired after user turn released")
	}
	endBack := func() func() {
		end, ok := s.BackgroundTurn(t.Context())
		if !ok {
			t.Fatal("background turn refused on idle session")
		}
		return end
	}()
	release := endBack
	userDone := make(chan struct{})
	go func() {
		end := s.UserTurn()
		end()
		close(userDone)
	}()
	select {
	case <-userDone:
		t.Fatal("user turn acquired while background turn held")
	case <-time.After(150 * time.Millisecond):
	}
	release()
	select {
	case <-userDone:
	case <-time.After(2 * time.Second):
		t.Fatal("user turn not acquired after background turn released")
	}
}

func TestListFolderIncremental(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@tuta.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@tuta.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var entriesCalls, mailCalls atomic.Int64
	s := newSession()
	s.st = st
	s.acct = mail.Account{ID: 1}
	s.folders = []tutaFolder{{name: "Inbox", entriesID: "entriesInbox"}}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailsetentry/"):
			entriesCalls.Add(1)
			return jsonResponse(r, `[
				{"1452":["entriesInbox","entryElem1"],"1456":[["mailList1","mailElem1"]]},
				{"1452":["entriesInbox","entryElem2"],"1456":[["mailList1","mailElem2"]]}
			]`), nil
		case strings.Contains(r.URL.Path, "/mail/"):
			mailCalls.Add(1)
			return jsonResponse(r, `[
				{"99":["mailList1","mailElem1"],"107":"1700000000000"},
				{"99":["mailList1","mailElem2"],"107":"1700000001000"}
			]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}

	fc, err := s.listFolderLocked("Inbox")
	if err != nil {
		t.Fatalf("first listFolderLocked: %v", err)
	}
	if fc.total != 2 {
		t.Fatalf("total = %d, want 2", fc.total)
	}
	if entriesCalls.Load() != 1 || mailCalls.Load() != 1 {
		t.Fatalf("first listing calls entries=%d mail=%d, want 1/1", entriesCalls.Load(), mailCalls.Load())
	}

	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("second listFolderLocked: %v", err)
	}
	if entriesCalls.Load() != 1 || mailCalls.Load() != 1 {
		t.Fatalf("cached listing made calls entries=%d mail=%d, want still 1/1", entriesCalls.Load(), mailCalls.Load())
	}

	s.markMailDirty("mailElem1")
	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("dirty listFolderLocked: %v", err)
	}
	if entriesCalls.Load() != 1 || mailCalls.Load() != 2 {
		t.Fatalf("dirty refresh calls entries=%d mail=%d, want 1/2", entriesCalls.Load(), mailCalls.Load())
	}

	s.markEntriesStale("entriesInbox")
	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("stale listFolderLocked: %v", err)
	}
	if entriesCalls.Load() != 2 || mailCalls.Load() != 2 {
		t.Fatalf("entries-stale listing calls entries=%d mail=%d, want 2/2 (membership relisted, metas reused)", entriesCalls.Load(), mailCalls.Load())
	}

	s.InvalidateCaches()
	s.folders = []tutaFolder{{name: "Inbox", entriesID: "entriesInbox"}}
	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("invalidated listFolderLocked: %v", err)
	}
	if entriesCalls.Load() != 3 || mailCalls.Load() != 3 {
		t.Fatalf("full listing calls entries=%d mail=%d, want 3/3", entriesCalls.Load(), mailCalls.Load())
	}
}

func TestDirtyMarksSurviveOtherFolderSync(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@tuta.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@tuta.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	var sentMailCalls atomic.Int64
	s := newSession()
	s.st = st
	s.acct = mail.Account{ID: 1}
	s.folders = []tutaFolder{
		{name: "Inbox", entriesID: "entriesInbox"},
		{name: "Sent", entriesID: "entriesSent"},
	}
	s.rest.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/mailsetentry/entriesInbox"):
			return jsonResponse(r, `[{"1452":["entriesInbox","eIn1"],"1456":[["mailListIn","mailIn1"]]}]`), nil
		case strings.Contains(r.URL.Path, "/mailsetentry/entriesSent"):
			return jsonResponse(r, `[{"1452":["entriesSent","eSent1"],"1456":[["mailListSent","mailSent1"]]}]`), nil
		case strings.Contains(r.URL.Path, "/mail/mailListIn"):
			return jsonResponse(r, `[{"99":["mailListIn","mailIn1"],"107":"1700000000000"}]`), nil
		case strings.Contains(r.URL.Path, "/mail/mailListSent"):
			sentMailCalls.Add(1)
			return jsonResponse(r, `[{"99":["mailListSent","mailSent1"],"107":"1700000000000"}]`), nil
		}
		return jsonResponse(r, `{}`), nil
	})}
	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("prime inbox: %v", err)
	}
	if _, err := s.listFolderLocked("Sent"); err != nil {
		t.Fatalf("prime sent: %v", err)
	}
	if sentMailCalls.Load() != 1 {
		t.Fatalf("sent mail calls after prime = %d, want 1", sentMailCalls.Load())
	}
	s.markMailDirty("mailSent1")
	if _, err := s.listFolderLocked("Inbox"); err != nil {
		t.Fatalf("inbox fast path: %v", err)
	}
	if sentMailCalls.Load() != 1 {
		t.Fatalf("inbox sync fetched sent mails, calls = %d, want 1", sentMailCalls.Load())
	}
	if !s.peekDirtyMails()["mailSent1"] {
		t.Fatal("dirty mark for mailSent1 was consumed by the inbox sync")
	}
	if _, err := s.listFolderLocked("Sent"); err != nil {
		t.Fatalf("sent refresh: %v", err)
	}
	if sentMailCalls.Load() != 2 {
		t.Fatalf("sent mail calls after its own sync = %d, want 2 (dirty refetch)", sentMailCalls.Load())
	}
	if s.peekDirtyMails()["mailSent1"] {
		t.Fatal("dirty mark for mailSent1 not cleared after sent refresh")
	}
}
