package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"namakani/internal/store"
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

func TestRetryConflictCarriesLatestStatus(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	insertOpRow(t, f, 77, store.OpMove, store.OpDone, 10)
	rec := callOpHandler(f, f.srv.handleRetryOp, "77")
	if rec.Code != http.StatusConflict {
		t.Fatalf("retry done op: code = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != store.OpDone {
		t.Fatalf("retry done op: op_status = %q, want %q", got, store.OpDone)
	}
}

func TestRetryMissingCarriesGone(t *testing.T) {
	f := newDrainFixture(t)
	rec := callOpHandler(f, f.srv.handleRetryOp, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("retry missing op: code = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != "gone" {
		t.Fatalf("retry missing op: op_status = %q, want gone", got)
	}
}

func TestDiscardConflictCarriesLatestStatus(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	insertOpRow(t, f, 78, store.OpMove, store.OpDone, 10)
	rec := callOpHandler(f, f.srv.handleDiscardOp, "78")
	if rec.Code != http.StatusConflict {
		t.Fatalf("discard done op: code = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != store.OpDone {
		t.Fatalf("discard done op: op_status = %q, want %q", got, store.OpDone)
	}
}

func TestDiscardMissingCarriesGone(t *testing.T) {
	f := newDrainFixture(t)
	rec := callOpHandler(f, f.srv.handleDiscardOp, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("discard missing op: code = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != "gone" {
		t.Fatalf("discard missing op: op_status = %q, want gone", got)
	}
}

func TestSendNowConflictCarriesLatestStatus(t *testing.T) {
	f := newDrainFixture(t)
	insertOpRow(t, f, 79, store.OpSend, store.OpDone, 0)
	rec := callOpHandler(f, f.srv.handleSendNowOp, "79")
	if rec.Code != http.StatusConflict {
		t.Fatalf("send-now done op: code = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != store.OpDone {
		t.Fatalf("send-now done op: op_status = %q, want %q", got, store.OpDone)
	}
}

func TestUndoMissingCarriesGone(t *testing.T) {
	f := newDrainFixture(t)
	rec := callOpHandler(f, f.srv.handleUndoOp, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("undo missing op: code = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if got := opStatusOf(t, rec); got != "gone" {
		t.Fatalf("undo missing op: op_status = %q, want gone", got)
	}
}

func TestPendingListCarriesQueueFields(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	insertOpRow(t, f, 80, store.OpMove, store.OpPending, 10)
	req := httptest.NewRequest(http.MethodGet, "/api/ops/pending", nil)
	rec := httptest.NewRecorder()
	f.srv.handleListPending(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list: code = %d (%s)", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("pending list not json: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("pending list rows = %d, want 1 (%s)", len(rows), rec.Body.String())
	}
	r0 := rows[0]
	if r0["attempts"] != float64(2) {
		t.Fatalf("pending row attempts = %v, want 2", r0["attempts"])
	}
	if r0["next_attempt"] != float64(4102444800) {
		t.Fatalf("pending row next_attempt = %v, want 4102444800", r0["next_attempt"])
	}
	if r0["account_label"] != "a" {
		t.Fatalf("pending row account_label = %v, want a", r0["account_label"])
	}
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
