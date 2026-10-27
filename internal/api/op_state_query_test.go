package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type opStateBody struct {
	OpID      int64  `json:"op_id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	NotBefore int64  `json:"not_before"`
	Sealed    bool   `json:"sealed"`
	Sent      bool   `json:"sent"`
}

func callGetOp(f *drainFixture, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/ops/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	f.srv.handleGetOp(rec, req)
	return rec
}

func TestGetOpReportsSendTerminalState(t *testing.T) {
	f := newDrainFixture(t)
	if _, err := f.st.DB.Exec(`INSERT INTO ops(id, account_id, type, message_id, status, attempts, next_attempt, not_before, payload)
		VALUES(77, 1, 'send', 0, 'done', 1, 0, 1700000000, '{"req":{},"sent":true}')`); err != nil {
		t.Fatalf("insert op: %v", err)
	}
	rec := callGetOp(f, "77")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body opStateBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body.OpID != 77 || body.Type != "send" || body.Status != "done" || !body.Sent {
		t.Fatalf("body = %+v, want op 77 send done sent=true: a client rebuilding its send bar from server state must learn the send finished", body)
	}
}

func TestGetOpReportsPendingSendWindow(t *testing.T) {
	f := newDrainFixture(t)
	if _, err := f.st.DB.Exec(`INSERT INTO ops(id, account_id, type, message_id, status, attempts, next_attempt, not_before, payload)
		VALUES(78, 1, 'send', 0, 'pending', 0, 0, 1700000100, '{"req":{},"sent":false}')`); err != nil {
		t.Fatalf("insert op: %v", err)
	}
	rec := callGetOp(f, "78")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body opStateBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body.Status != "pending" || body.Sent || body.NotBefore != 1700000100 {
		t.Fatalf("body = %+v, want pending sent=false not_before=1700000100 so the client keeps the undo countdown running", body)
	}
}

func TestGetOpMissingReturns404(t *testing.T) {
	f := newDrainFixture(t)
	rec := callGetOp(f, "999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown op", rec.Code)
	}
}
