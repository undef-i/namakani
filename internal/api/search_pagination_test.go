package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func (f *drainFixture) searchPage(t *testing.T, q string, limit, offset int) []threadOut {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/search?q=%s&limit=%d&offset=%d", q, limit, offset), nil)
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

func TestSearchPaginationCoversAllResultsWithoutOverlap(t *testing.T) {
	f := newDrainFixture(t)
	dates := map[int64]int64{10: 500, 11: 400, 12: 300, 13: 300, 14: 100}
	for id, ts := range dates {
		f.addMessage(t, id, 1, "INBOX", uint32(100+id), 0)
		if _, err := f.st.DB.Exec("UPDATE messages SET date_ts = ? WHERE id = ?", ts, id); err != nil {
			t.Fatalf("set date: %v", err)
		}
		if _, err := f.st.DB.Exec(`INSERT INTO messages_fts(rowid, subject, senders, recipients, body)
			VALUES(?, 'weekly digest', '{}', '[]', '')`, id); err != nil {
			t.Fatalf("seed fts: %v", err)
		}
	}
	seen := make(map[int64]bool)
	var order []int64
	for offset := 0; offset < 6; offset += 2 {
		page := f.searchPage(t, "weekly", 2, offset)
		for _, row := range page {
			if seen[row.ID] {
				t.Fatalf("message %d appeared on two pages; offset paging over date ties must be stable", row.ID)
			}
			seen[row.ID] = true
			order = append(order, row.ID)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged through %d distinct messages, want all 5 (order %v)", len(seen), order)
	}
	want := []int64{10, 11, 13, 12, 14}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("result order = %v, want %v (date_ts desc, id desc)", order, want)
		}
	}
	last := f.searchPage(t, "weekly", 2, 4)
	if len(last) != 1 || last[0].ID != 14 {
		t.Fatalf("final page = %+v, want exactly message 14", last)
	}
}
