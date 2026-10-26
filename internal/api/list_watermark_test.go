package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func listHeaderVer(t *testing.T, f *drainFixture, url string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	f.srv.handleListMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s: status %d body %s", url, rec.Code, rec.Body.String())
	}
	return rec.Header().Get("X-WM-Ver")
}

func TestListMessagesCarriesPlacementWatermark(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 110, 0)
	f.addMessage(t, 11, 1, "INBOX", 111, 0)
	if got := listHeaderVer(t, f, "/api/messages?special=inbox&limit=60&offset=0&threaded=1"); got != "2" {
		t.Fatalf("threaded watermark = %q, want 2 (two inserts are two placements)", got)
	}
	if _, err := f.st.DB.Exec(`UPDATE messages SET flags = 1 WHERE id = 10`); err != nil {
		t.Fatalf("flags update: %v", err)
	}
	if got := listHeaderVer(t, f, "/api/messages?special=inbox&limit=60&offset=0"); got != "2" {
		t.Fatalf("watermark after flags update = %q, want 2 (flags are not placements)", got)
	}
	if _, err := f.st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = 11`); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := listHeaderVer(t, f, "/api/messages?special=inbox&limit=60&offset=0"); got != "3" {
		t.Fatalf("watermark after move = %q, want 3", got)
	}
}

func TestBroadcastsCarryPlacementVersions(t *testing.T) {
	f := newDrainFixture(t)
	ch, unsub := f.srv.hub.subscribe()
	defer unsub()
	f.addMessage(t, 20, 1, "INBOX", 120, 0)
	f.srv.broadcastMessages([]int64{20})
	var ev map[string]any
	if err := json.Unmarshal(<-ch, &ev); err != nil {
		t.Fatalf("decode messages event: %v", err)
	}
	items, ok := ev["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("messages event items = %v, want exactly the broadcast row", ev["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["pv"] != float64(1) {
		t.Fatalf("messages event item = %v, want pv 1 for the first placement", item)
	}
	if _, err := f.st.DB.Exec(`UPDATE messages SET folder_id = 2 WHERE id = 20`); err != nil {
		t.Fatalf("move: %v", err)
	}
	f.srv.broadcastRemoved(1, []int64{20})
	if err := json.Unmarshal(<-ch, &ev); err != nil {
		t.Fatalf("decode removed event: %v", err)
	}
	pvs, ok := ev["pvs"].(map[string]any)
	if !ok || pvs["20"] != float64(2) {
		t.Fatalf("removed event pvs = %v, want id 20 at version 2 after the move", ev["pvs"])
	}
	if _, err := f.st.DB.Exec(`DELETE FROM messages WHERE id = 20`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	f.srv.broadcastRemoved(2, []int64{20})
	if err := json.Unmarshal(<-ch, &ev); err != nil {
		t.Fatalf("decode removed event: %v", err)
	}
	pvs, ok = ev["pvs"].(map[string]any)
	if !ok || pvs["20"] != float64(3) {
		t.Fatalf("removed event pvs after delete = %v, want tombstone at version 3", ev["pvs"])
	}
}
