package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"namakani/internal/store"
)

type pendingItem struct {
	OpID         int64  `json:"op_id"`
	ClientKey    string `json:"client_key"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	MessageID    int64  `json:"message_id"`
	Subject      string `json:"subject"`
	To           string `json:"to"`
	PriorSpecial string `json:"prior_special"`
	ToSpecial    string `json:"to_special"`
	NotBefore    int64  `json:"not_before"`
}

func listPending(t *testing.T, f *drainFixture) []pendingItem {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleListPending(rec, httptest.NewRequest(http.MethodGet, "/api/ops/pending", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list status = %d, body %s", rec.Code, rec.Body.String())
	}
	var items []pendingItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode pending list: %v (body %s)", err, rec.Body.String())
	}
	return items
}

func TestPendingListShowsUnexecutedOpsOnly(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	now := time.Now().Unix()
	moveID, err := f.st.EnqueueMove(1, 10, 2, "pk-move")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	sendID, err := f.st.EnqueueSend(1, `{"req":{"account_id":1,"to":"bob@x.test","subject":"later hi"}}`, now+600, "pk-send")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	doneID, err := f.st.EnqueueExpunge(1, 10, "pk-done")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'done', finished_at = ? WHERE id = ?", now, doneID); err != nil {
		t.Fatalf("finish expunge: %v", err)
	}
	items := listPending(t, f)
	if len(items) != 2 {
		t.Fatalf("pending items = %d, want 2 (the done op must not be listed)", len(items))
	}
	byID := map[int64]pendingItem{}
	for _, it := range items {
		byID[it.OpID] = it
	}
	if _, ok := byID[moveID]; !ok {
		t.Fatalf("pending list is missing the move op %d", moveID)
	}
	if _, ok := byID[sendID]; !ok {
		t.Fatalf("pending list is missing the deferred send op %d", sendID)
	}
}

func byKeys(t *testing.T, f *drainFixture, keys ...string) []byKeyOut {
	t.Helper()
	body := `{"keys":[` + strings.Join(func() []string {
		q := make([]string, len(keys))
		for i, k := range keys {
			b, _ := json.Marshal(k)
			q[i] = string(b)
		}
		return q
	}(), ",") + `]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/ops/by-keys", strings.NewReader(body))
	f.srv.handleOpsByKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-keys status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out []byKeyOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode by-keys: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func TestOpsByKeysResolvesLiveAndPurgedOps(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	liveID, err := f.st.EnqueueExpunge(1, 10, "bk-live")
	if err != nil {
		t.Fatalf("enqueue live: %v", err)
	}
	goneID, err := f.st.EnqueueSend(1, `{"req":{"account_id":1,"to":"a@x.test","subject":"s"}}`, 0, "bk-gone")
	if err != nil {
		t.Fatalf("enqueue gone: %v", err)
	}
	now := time.Now().Unix()
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'done', finished_at = ? WHERE id = ?", now-8*86400, goneID); err != nil {
		t.Fatalf("age send: %v", err)
	}
	if err := f.st.PurgeTerminalOps(now); err != nil {
		t.Fatalf("purge: %v", err)
	}
	out := byKeys(t, f, "bk-live", "bk-gone", "bk-unknown")
	if len(out) != 2 {
		t.Fatalf("by-keys returned %d entries, want 2 (unknown keys are omitted)", len(out))
	}
	byKey := map[string]byKeyOut{}
	for _, e := range out {
		byKey[e.ClientKey] = e
	}
	if e := byKey["bk-live"]; e.OpID != liveID || e.Status != store.OpPending {
		t.Fatalf("live key entry = %+v, want op %d pending", e, liveID)
	}
	if e := byKey["bk-gone"]; e.OpID != goneID || e.Status != store.OpDone {
		t.Fatalf("purged key entry = %+v, want op %d done (the tombstone must keep the key resolvable)", e, goneID)
	}
}
