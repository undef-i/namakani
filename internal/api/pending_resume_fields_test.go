package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"namakani/internal/store"
)

// D1: restoring the send bar after a reload decides from this list
// alone which sends still have a live undo entry. The list must
// therefore carry the two facts the decision turns on — whether the
// op is already sealed and whether its payload already landed the
// Sent marker — across every still-active state, including a send
// whose not_before has passed without a claim and a claimed send
// waiting behind a bulk wave. Before the fix neither fact was in the
// payload, and the frontend filtered the rest away by time.

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
	// A send whose time has come but that no wave has claimed yet:
	// still pending, still undoable, and dropped by the old resume
	// filter for the sole reason that not_before <= now.
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
	if it, ok := items[waitingID]; !ok || it.Sealed || it.Sent || it.Status != store.OpPending {
		t.Fatalf("waiting send in pending list = %+v (present %v), want pending, unsealed, unsent", it, ok)
	}
	if it, ok := items[overdueID]; !ok || it.Sealed || it.Sent || it.Status != store.OpPending {
		t.Fatalf("overdue unclaimed send in pending list = %+v (present %v), want pending, unsealed, unsent: its undo entry must be restorable", it, ok)
	}
	if it, ok := items[sealedID]; !ok || !it.Sealed || it.Sent || it.Status != store.OpInFlight {
		t.Fatalf("sealed send in pending list = %+v (present %v), want in_flight with sealed set", it, ok)
	}
	if it, ok := items[sentID]; !ok || !it.Sent || it.Status != store.OpInFlight {
		t.Fatalf("sent send in pending list = %+v (present %v), want in_flight with sent set", it, ok)
	}
}
