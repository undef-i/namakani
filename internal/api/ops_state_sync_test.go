package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	syncmgr "namakani/internal/sync"
)

func insertOpRow(t *testing.T, f *drainFixture, id int64, typ, status string, messageID int64) {
	t.Helper()
	if _, err := f.st.DB.Exec(`INSERT INTO ops(id, account_id, type, message_id, status, attempts, next_attempt, payload)
		VALUES(?, 1, ?, ?, ?, 2, 4102444800, '{}')`, id, typ, messageID, status); err != nil {
		t.Fatalf("insert op: %v", err)
	}
}

func callOpHandler(f *drainFixture, h func(http.ResponseWriter, *http.Request), id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/ops/"+id+"/x", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func opStatusOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		OpStatus string `json:"op_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, rec.Body.String())
	}
	return body.OpStatus
}

func TestOnSyncChangeBroadcastsOps(t *testing.T) {
	f := newDrainFixture(t)
	ch, unsub := f.srv.hub.subscribe()
	defer unsub()
	f.srv.onSyncChange(syncmgr.Change{FolderID: 1})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-ch:
			var ev map[string]any
			if err := json.Unmarshal(data, &ev); err != nil {
				continue
			}
			if ev["type"] == "ops" {
				return
			}
		case <-deadline:
			t.Fatal("no ops broadcast from onSyncChange")
		}
	}
}
