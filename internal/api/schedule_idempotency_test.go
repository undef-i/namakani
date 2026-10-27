package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func (f *drainFixture) scheduleWithKey(t *testing.T, body string) (int64, int) {
	t.Helper()
	req := f.authedReq(http.MethodPost, "/api/send/schedule", body)
	rec := httptest.NewRecorder()
	f.srv.handleScheduleSend(rec, req)
	var resp struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.ID, rec.Code
}

func (f *drainFixture) countSendOps(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE type = 'send'").Scan(&n); err != nil {
		t.Fatalf("count send ops: %v", err)
	}
	return n
}

func TestScheduleSendWithClientKeyDedupesRetry(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	sendAt := time.Now().Add(2 * time.Hour).Unix()
	body := fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"later","text":"body","send_at":%d,"client_key":"sched-key-1"}`, sendAt)
	id1, code1 := f.scheduleWithKey(t, body)
	if code1 != http.StatusCreated || id1 >= 0 {
		t.Fatalf("first schedule = (%d, %d), want (negative id, 201)", id1, code1)
	}
	id2, code2 := f.scheduleWithKey(t, body)
	if code2 != http.StatusCreated || id2 != id1 {
		t.Fatalf("retry schedule = (%d, %d), want the same op (%d, 201): a retried submission must converge on the first op, not enqueue a twin", id2, code2, id1)
	}
	if n := f.countSendOps(t); n != 1 {
		t.Fatalf("send ops after a duplicated schedule submission = %d, want 1", n)
	}
}

func TestScheduleSendWithClientKeyConflictOnDifferentPayload(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	sendAt := time.Now().Add(2 * time.Hour).Unix()
	body := fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"later","text":"body","send_at":%d,"client_key":"sched-key-2"}`, sendAt)
	if _, code := f.scheduleWithKey(t, body); code != http.StatusCreated {
		t.Fatalf("first schedule status = %d, want 201", code)
	}
	changed := fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"CHANGED","text":"body","send_at":%d,"client_key":"sched-key-2"}`, sendAt)
	if _, code := f.scheduleWithKey(t, changed); code == http.StatusCreated {
		t.Fatal("reusing a schedule client key for a different letter was accepted; the key conflict must surface like it does for immediate sends")
	}
	if n := f.countSendOps(t); n != 1 {
		t.Fatalf("send ops after a conflicting key reuse = %d, want 1", n)
	}
}

func TestScheduleFromDraftDedupesByDraftIdentity(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 50, 4, "Drafts", 500, 0)
	sendAt := time.Now().Add(2 * time.Hour).Unix()
	body := fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"sched","text":"body","send_at":%d,"draft_id":50}`, sendAt)
	id1, code1 := f.scheduleWithKey(t, body)
	if code1 != http.StatusCreated || id1 >= 0 {
		t.Fatalf("first schedule from draft = (%d, %d), want (negative id, 201)", id1, code1)
	}
	id2, code2 := f.scheduleWithKey(t, body)
	if code2 != http.StatusCreated || id2 != id1 {
		t.Fatalf("resubmitted schedule from the same draft = (%d, %d), want the same op (%d, 201)", id2, code2, id1)
	}
	if n := f.countSendOps(t); n != 1 {
		t.Fatalf("send ops after scheduling the same draft twice = %d, want 1", n)
	}
}
