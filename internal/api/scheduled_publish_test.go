package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"namakani/internal/store"
)

type rawEvent struct {
	Type   string `json:"type"`
	OpID   int64  `json:"op_id"`
	Status string `json:"status"`
}

func (f *drainFixture) collectRawEvents(t *testing.T) func() []rawEvent {
	t.Helper()
	ch, unsub := f.srv.hub.subscribe()
	t.Cleanup(unsub)
	return func() []rawEvent {
		var out []rawEvent
		for {
			select {
			case data := <-ch:
				var ev rawEvent
				if err := json.Unmarshal(data, &ev); err == nil {
					out = append(out, ev)
				}
			default:
				return out
			}
		}
	}
}

func hasScheduledEvent(evs []rawEvent) bool {
	for _, ev := range evs {
		if ev.Type == "scheduled" {
			return true
		}
	}
	return false
}

func (f *drainFixture) scheduleSend(t *testing.T, draftID int64, sendAt int64) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"sched","text":"body","send_at":%d`, sendAt)
	if draftID > 0 {
		body += fmt.Sprintf(`,"draft_id":%d`, draftID)
	}
	body += `}`
	req := f.authedReq(http.MethodPost, "/api/send/schedule", body)
	rec := httptest.NewRecorder()
	f.srv.handleScheduleSend(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("schedule status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.ID >= 0 {
		t.Fatalf("schedule response = %s, want a negative row id", rec.Body.String())
	}
	return -resp.ID
}

func (f *drainFixture) scheduledRow(t *testing.T, opID int64) map[string]any {
	t.Helper()
	req := f.authedReq(http.MethodGet, "/api/scheduled", "")
	rec := httptest.NewRecorder()
	f.srv.handleListScheduled(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list scheduled status = %d, body %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list scheduled body: %v", err)
	}
	for _, r := range rows {
		if int64(r["id"].(float64)) == -opID {
			return r
		}
	}
	return nil
}

func TestScheduleSendPublishesScheduledEvent(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	events := f.collectRawEvents(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	if !hasScheduledEvent(events()) {
		t.Fatal("scheduling a send published no 'scheduled' event; the scheduled view keeps serving its stale cache until a full reload")
	}
	if row := f.scheduledRow(t, opID); row == nil {
		t.Fatal("the scheduled op is missing from the scheduled list")
	}
}

func TestUpdateScheduledPublishesScheduledEvent(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	events := f.collectRawEvents(t)
	newAt := time.Now().Add(5 * time.Hour).Unix()
	req := f.authedReq(http.MethodPut, fmt.Sprintf("/api/scheduled/%d", -opID),
		fmt.Sprintf(`{"account_id":1,"to":"b@x.test","subject":"sched","text":"body","send_at":%d}`, newAt))
	req.SetPathValue("id", fmt.Sprint(-opID))
	rec := httptest.NewRecorder()
	f.srv.handleUpdateScheduled(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update scheduled status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !hasScheduledEvent(events()) {
		t.Fatal("rescheduling published no 'scheduled' event; the view keeps showing the old time until a full reload")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.NotBefore != newAt {
		t.Fatalf("not_before = %d, want %d", o.NotBefore, newAt)
	}
}

func TestScheduleFromDraftConsumesTheDraft(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 50, 4, "Drafts", 500, 0)
	events := f.collectRawEvents(t)
	opID := f.scheduleSend(t, 50, time.Now().Add(2*time.Hour).Unix())
	if !hasScheduledEvent(events()) {
		t.Fatal("scheduling from a draft published no 'scheduled' event")
	}
	var expunges int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE type = 'expunge' AND message_id = 50").Scan(&expunges); err != nil {
		t.Fatalf("count expunge ops: %v", err)
	}
	if expunges == 0 {
		t.Fatal("scheduling from a draft left the draft in place: no expunge op was enqueued for the source draft, so it stays in Drafts next to its scheduled copy")
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	var rows int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 50").Scan(&rows); err != nil {
		t.Fatalf("count draft row: %v", err)
	}
	if rows != 0 {
		t.Fatal("the source draft row survived the drain; scheduling must consume it exactly like a completed send does")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get send op: %v", err)
	}
	if o.Status != store.OpPending {
		t.Fatalf("send op status = %q, want pending (its time has not come)", o.Status)
	}
}

func (f *drainFixture) cancelScheduled(t *testing.T, opID int64, purge bool) {
	t.Helper()
	path := fmt.Sprintf("/api/scheduled/%d", -opID)
	if purge {
		path += "?purge=1"
	}
	req := f.authedReq(http.MethodDelete, path, "")
	req.SetPathValue("id", fmt.Sprint(-opID))
	rec := httptest.NewRecorder()
	f.srv.handleCancelScheduled(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel scheduled status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestCancelScheduledReturnsDraftAndPublishes(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	events := f.collectRawEvents(t)
	f.cancelScheduled(t, opID, false)
	if !hasScheduledEvent(events()) {
		t.Fatal("cancelling a scheduled send published no 'scheduled' event")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
	var drafts int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE folder_id = 4").Scan(&drafts); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if drafts != 1 {
		t.Fatalf("drafts after cancel = %d, want 1 (the letter returns to Drafts)", drafts)
	}
}

func TestPurgeScheduledDeletesLetterAndPayload(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	dir := outgoingDir(f.st.DataDir(), opID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("outgoing payload dir missing before purge: %v", err)
	}
	events := f.collectRawEvents(t)
	f.cancelScheduled(t, opID, true)
	if !hasScheduledEvent(events()) {
		t.Fatal("purging a scheduled send published no 'scheduled' event")
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("outgoing payload dir still present after purge (stat err = %v); a true delete must destroy the letter, not park it", err)
	}
	var drafts int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE folder_id = 4").Scan(&drafts); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if drafts != 0 {
		t.Fatalf("drafts after purge = %d, want 0 (a true delete must not resurrect the letter as a draft)", drafts)
	}
}
