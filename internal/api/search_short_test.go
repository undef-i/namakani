package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func (f *drainFixture) search(t *testing.T, q string) []threadOut {
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

func TestSearchShortTermFallsBackToSubjectAndSenders(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.addMessage(t, 11, 1, "INBOX", 101, 0)
	if _, err := f.st.DB.Exec(`UPDATE messages SET subject = 'AI weekly digest' WHERE id = 10`); err != nil {
		t.Fatalf("set subject: %v", err)
	}
	if _, err := f.st.DB.Exec(`UPDATE messages SET subject = 'weekly report' WHERE id = 11`); err != nil {
		t.Fatalf("set subject: %v", err)
	}
	got := f.search(t, "AI")
	if len(got) != 1 || got[0].ID != 10 {
		t.Fatalf("search AI = %+v, want exactly message 10: two-letter terms can never match a trigram index, so they must be searched in subjects and addresses instead of returning nothing", got)
	}
}

func TestSearchLongTermStillUsesFTS(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	if _, err := f.st.DB.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
		VALUES(10, 'AI weekly digest', '{}', '[]', '')`); err != nil {
		t.Fatalf("seed fts: %v", err)
	}
	got := f.search(t, "weekly")
	if len(got) != 1 || got[0].ID != 10 {
		t.Fatalf("search weekly = %+v, want exactly message 10", got)
	}
}
