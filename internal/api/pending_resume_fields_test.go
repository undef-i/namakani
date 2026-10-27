package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type pendingResumeItem struct {
	OpID   int64  `json:"op_id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Sealed bool   `json:"sealed"`
	Sent   bool   `json:"sent"`
}

func listPendingResume(t *testing.T, f *drainFixture) map[int64]pendingResumeItem {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleListPending(rec, httptest.NewRequest(http.MethodGet, "/api/ops/pending", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pending list status = %d, body %s", rec.Code, rec.Body.String())
	}
	var items []pendingResumeItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode pending list: %v (body %s)", err, rec.Body.String())
	}
	out := map[int64]pendingResumeItem{}
	for _, it := range items {
		out[it.OpID] = it
	}
	return out
}

func TestListPendingExposesSealedAndSentForResume(t *testing.T) {
	f := newDrainFixture(t)
	now := time.Now().Unix()
	payload := `{"req":{"account_id":1,"to":"bob@x.test","subject":"hi"}}`
	waitingID, err := f.st.EnqueueSend(1, payload, now+30, "resume-waiting")
	if err != nil {
		t.Fatalf("enqueue waiting send: %v", err)
	}
	overdueID, err := f.st.EnqueueSend(1, payload, now-10, "resume-overdue")
	if err != nil {
		t.Fatalf("enqueue overdue send: %v", err)
	}
	sealedID, err := f.st.EnqueueSend(1, payload, now-10, "resume-sealed")
	if err != nil {
		t.Fatalf("enqueue sealed send: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'in_flight', claim_token = 42 WHERE id = ?", sealedID); err != nil {
		t.Fatalf("claim sealed send: %v", err)
	}
	if sealed, _, err := f.st.SealSendForDelivery(sealedID, 42, 43); err != nil || !sealed {
		t.Fatalf("seal = (%v, %v), want (true, nil)", sealed, err)
	}
	sentID, err := f.st.EnqueueSend(1, payload, now-10, "resume-sent")
	if err != nil {
		t.Fatalf("enqueue sent send: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'in_flight', claim_token = 42 WHERE id = ?", sentID); err != nil {
		t.Fatalf("claim sent send: %v", err)
	}
	sentPayload, err := json.Marshal(sendOpPayload{Sent: true})
	if err != nil {
		t.Fatalf("marshal sent payload: %v", err)
	}
	if applied, err := f.st.UpdateOpPayload(sentID, string(sentPayload)); err != nil || !applied {
		t.Fatalf("mark sent payload = (%v, %v), want (true, nil)", applied, err)
	}
	items := listPendingResume(t, f)
	if _, ok := items[waitingID]; !ok {
		t.Fatal("waiting send missing from the pending list")
	}
	if _, ok := items[overdueID]; !ok {
		t.Fatal("overdue unclaimed send missing from the pending list: its undo entry must be restorable")
	}
	if _, ok := items[sealedID]; !ok {
		t.Fatal("sealed send missing from the pending list")
	}
	if _, ok := items[sentID]; !ok {
		t.Fatal("sent send missing from the pending list")
	}
}
