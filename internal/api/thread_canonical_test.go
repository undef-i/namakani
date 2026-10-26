package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

type threadFixture struct {
	srv *Server
	st  *store.Store
}

func newThreadFixture(t *testing.T) *threadFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at) VALUES
		(1, 'p', 'p@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'p@x.test', '', 0),
		(2, 'i', 'i@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'i@x.test', '', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(11, 1, 'INBOX', 'inbox'), (12, 1, 'Trash', 'trash'), (13, 1, 'All Mail', 'all'),
		(21, 2, 'INBOX', 'inbox'), (22, 2, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	seed := `INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key, subject, date_ts, flags) VALUES `
	rows := []string{
		`(101, 1, 11, 1, 'a1@x', 'root@x', 'Qzx root', 1000, 1)`,
		`(102, 1, 11, 2, 'a2@x', 'root@x', 'Re: Qzx root', 2000, 0)`,
		`(103, 1, 12, 3, 'a3@x', 'root@x', 'Re: Qzx root', 3000, 0)`,
		`(104, 1, 12, 4, 'a4@x', 'root@x', 'Re: Qzx root', 4000, 1)`,
		`(105, 1, 12, 5, 'a5@x', 'root@x', 'Re: Qzx root', 5000, 1)`,
		`(106, 1, 12, 6, 'a6@x', 'root@x', 'Re: Qzx root', 6000, 1)`,
		`(107, 1, 12, 7, 'a7@x', 'root@x', 'Re: Qzx root', 7000, 1)`,
		`(108, 1, 12, 8, 'a8@x', 'root@x', 'Re: Qzx root', 8000, 1)`,
		`(109, 1, 12, 9, 'a9@x', 'root@x', 'Re: Qzx root', 9000, 1)`,
		`(201, 1, 13, 201, 'a1@x', 'root@x', 'Qzx root', 1000, 1)`,
		`(202, 1, 13, 202, 'a2@x', 'root@x', 'Re: Qzx root', 2000, 0)`,
		`(203, 1, 13, 203, 'a3@x', 'root@x', 'Re: Qzx root', 3000, 0)`,
		`(204, 1, 13, 204, 'a4@x', 'root@x', 'Re: Qzx root', 4000, 1)`,
		`(205, 1, 13, 205, 'a5@x', 'root@x', 'Re: Qzx root', 5000, 1)`,
		`(206, 1, 13, 206, 'a6@x', 'root@x', 'Re: Qzx root', 6000, 1)`,
		`(207, 1, 13, 207, 'a7@x', 'root@x', 'Re: Qzx root', 7000, 1)`,
		`(208, 1, 13, 208, 'a8@x', 'root@x', 'Re: Qzx root', 8000, 1)`,
		`(209, 1, 13, 209, 'a9@x', 'root@x', 'Re: Qzx root', 9000, 1)`,
		`(210, 1, 13, 210, 'solo@x', 'solo@x', 'Solo archived', 500, 0)`,
		`(301, 2, 21, 1, 'b1@x', 'root@x', 'Qzx root', 1000, 1)`,
		`(302, 2, 21, 2, 'b2@x', 'root@x', 'Re: Qzx root', 2000, 0)`,
		`(303, 2, 22, 3, 'b3@x', 'root@x', 'Re: Qzx root', 3000, 0)`,
		`(304, 2, 22, 4, 'b4@x', 'root@x', 'Re: Qzx root', 4000, 1)`,
		`(305, 2, 22, 5, 'b5@x', 'root@x', 'Re: Qzx root', 5000, 1)`,
		`(306, 2, 22, 6, 'b6@x', 'root@x', 'Re: Qzx root', 6000, 1)`,
		`(307, 2, 22, 7, 'b7@x', 'root@x', 'Re: Qzx root', 7000, 1)`,
		`(308, 2, 22, 8, 'b8@x', 'root@x', 'Re: Qzx root', 8000, 1)`,
		`(309, 2, 22, 9, 'b9@x', 'root@x', 'Re: Qzx root', 9000, 1)`,
	}
	for _, r := range rows {
		if _, err := st.DB.Exec(seed + r); err != nil {
			t.Fatalf("seed message %s: %v", r, err)
		}
	}
	if _, err := st.DB.Exec(`UPDATE messages SET body_state = 1 WHERE id = 101`); err != nil {
		t.Fatalf("seed body_state: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO bodies(message_id, text_body, html_body) VALUES(101, 'hello body', '')`); err != nil {
		t.Fatalf("seed body: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
		VALUES(101, 'Qzx root', '{}', '[]', '')`); err != nil {
		t.Fatalf("seed fts: %v", err)
	}
	srv := New(st, nil, syncmgr.NewManager(st))
	return &threadFixture{srv: srv, st: st}
}

func (f *threadFixture) listMessages(t *testing.T, rawq string) []messageOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/messages?"+rawq, nil)
	rec := httptest.NewRecorder()
	f.srv.handleListMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %q: status %d body %s", rawq, rec.Code, rec.Body.String())
	}
	var out []messageOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list %q: decode: %v", rawq, err)
	}
	return out
}

func (f *threadFixture) listThreads(t *testing.T, rawq string) []threadOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/messages?"+rawq, nil)
	rec := httptest.NewRecorder()
	f.srv.handleListMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %q: status %d body %s", rawq, rec.Code, rec.Body.String())
	}
	var out []threadOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list %q: decode: %v", rawq, err)
	}
	return out
}

func (f *threadFixture) search(t *testing.T, q string) []threadOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+q, nil)
	rec := httptest.NewRecorder()
	f.srv.handleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %q: status %d body %s", q, rec.Code, rec.Body.String())
	}
	var out []threadOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("search %q: decode: %v", q, err)
	}
	return out
}

func idsOf(out []messageOut) []int64 {
	ids := make([]int64, 0, len(out))
	for _, m := range out {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestThreadMembersFetchReturnsCanonicalRowsOnly(t *testing.T) {
	f := newThreadFixture(t)
	got := f.listMessages(t, "thread_all=1&account_id=1&thread_key=root%40x")
	want := []int64{101, 102, 103, 104, 105, 106, 107, 108, 109}
	ids := idsOf(got)
	if len(ids) != len(want) {
		t.Fatalf("thread members = %v, want exactly %v: one canonical row per message, trash members included, All Mail mirrors never separate members", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("thread members = %v, want %v in date order", ids, want)
		}
	}
}

func TestThreadMembersFetchIsAccountScoped(t *testing.T) {
	f := newThreadFixture(t)
	got := f.listMessages(t, "thread_all=1&account_id=2&thread_key=root%40x")
	want := []int64{301, 302, 303, 304, 305, 306, 307, 308, 309}
	ids := idsOf(got)
	if len(ids) != len(want) {
		t.Fatalf("account 2 thread members = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("account 2 thread members = %v, want %v", ids, want)
		}
	}
}

func TestThreadMembersAllOnlyRowStandsIn(t *testing.T) {
	f := newThreadFixture(t)
	got := f.listMessages(t, "thread_all=1&account_id=1&thread_key=solo%40x")
	if len(got) != 1 || got[0].ID != 210 {
		t.Fatalf("all-only thread members = %v, want exactly the All Mail row 210", idsOf(got))
	}
}

func TestThreadedListCountsWholeGroupInEveryScope(t *testing.T) {
	f := newThreadFixture(t)
	for _, special := range []string{"inbox", "trash", "all"} {
		out := f.listThreads(t, "threaded=1&special="+special)
		var hit *threadOut
		for i := range out {
			if out[i].AccountID == 1 && out[i].ThreadKey == "root@x" {
				hit = &out[i]
			}
		}
		if hit == nil {
			t.Fatalf("scope %s: thread root@x of account 1 missing from %v", special, out)
		}
		if hit.ThreadCount != 9 || hit.ThreadUnread != 2 {
			t.Fatalf("scope %s: count/unread = %d/%d, want 9/2: the group counts all unique members account-wide, mirrors merged, trash members included", special, hit.ThreadCount, hit.ThreadUnread)
		}
	}
}

func TestThreadDetailCountIsUniqueMembers(t *testing.T) {
	f := newThreadFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/messages/101", nil)
	req.SetPathValue("id", "101")
	rec := httptest.NewRecorder()
	f.srv.handleGetMessage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get message: status %d body %s", rec.Code, rec.Body.String())
	}
	var d messageDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("get message: decode: %v", err)
	}
	if d.ThreadCount != 9 {
		t.Fatalf("detail thread_count = %d, want 9 unique members, not 18 rows with mirrors", d.ThreadCount)
	}
}

func TestSearchThreadCountIsUniqueMembers(t *testing.T) {
	f := newThreadFixture(t)
	got := f.search(t, "Qzx")
	if len(got) != 1 || got[0].ID != 101 {
		t.Fatalf("search Qzx = %+v, want exactly message 101", got)
	}
	if got[0].ThreadCount != 9 {
		t.Fatalf("search thread_count = %d, want 9 unique members, not 18 rows with mirrors", got[0].ThreadCount)
	}
}
