package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"namakani/internal/store"
)

// D4: the scheduled view folded in_flight sends into "pending" and
// its cancel path ran CancelOp directly, which refuses in_flight ops
// outright — so cancelling a send that was claimed but not yet sealed
// (a window that can last the whole prep of a bulk wave) always hit a
// 409 even though the state machine could still honor the cancel
// through the undo path. These tests pin the honest shapes: real
// status out, cancel routed by state, sealed answered terminally.

// claimSendOp claims the send op the way production does: register a
// wave for the account first (drainAccount's registerWave), then claim
// and stamp the claims with the wave's token (stampClaims). The wave
// stays registered for the rest of the test, so the claim is a live
// wave's claim. A bare claim with no registered wave is an orphan by
// definition, and any background drain or watchdog pass may legally
// release it back to pending mid-test.
func (f *drainFixture) claimSendOp(t *testing.T, opID int64) *store.Op {
	t.Helper()
	var wave *drainWave
	deadline := time.Now().Add(5 * time.Second)
	for wave == nil {
		if w, ok := f.srv.registerWave(1); ok {
			wave = w
		} else if time.Now().After(deadline) {
			t.Fatalf("register wave: another drain wave for account 1 stayed registered")
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Cleanup(func() { f.srv.unregisterWave(1, wave) })
	if _, err := f.st.DB.Exec("UPDATE ops SET not_before = 0, next_attempt = 0 WHERE id = ?", opID); err != nil {
		t.Fatalf("force due: %v", err)
	}
	ops, err := f.st.ClaimDueOps(1, time.Now().Unix(), 10, store.OpSend)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	f.srv.stampClaims(wave, ops)
	for _, o := range ops {
		if o.ID == opID {
			return o
		}
	}
	t.Fatalf("send op %d was not claimed", opID)
	return nil
}

func (f *drainFixture) deleteScheduled(t *testing.T, opID int64, purge bool) *httptest.ResponseRecorder {
	t.Helper()
	path := fmt.Sprintf("/api/scheduled/%d", -opID)
	if purge {
		path += "?purge=1"
	}
	req := f.authedReq(http.MethodDelete, path, "")
	req.SetPathValue("id", fmt.Sprint(-opID))
	rec := httptest.NewRecorder()
	f.srv.handleCancelScheduled(rec, req)
	return rec
}

func (f *drainFixture) draftsCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE folder_id = 4").Scan(&n); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	return n
}

func TestListScheduledReportsInFlightHonestly(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	if row := f.scheduledRow(t, opID); row == nil || row["status"] != "pending" {
		t.Fatalf("scheduled row before claim = %v, want status pending", row)
	}
	f.claimSendOp(t, opID)
	row := f.scheduledRow(t, opID)
	if row == nil {
		t.Fatal("claimed scheduled op vanished from the scheduled list")
	}
	if row["status"] != "in_flight" {
		t.Fatalf("scheduled row status while claimed = %v, want in_flight (folding it into pending hides that its send time has come)", row["status"])
	}
	// The message-shaped projection behind the scheduled view must
	// carry the same truth.
	rec := httptest.NewRecorder()
	f.srv.listScheduledAsMessages(rec, url.Values{}, 100, 0)
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("scheduled messages body: %v", err)
	}
	found := false
	for _, r := range rows {
		if int64(r["id"].(float64)) == -opID {
			found = true
			if r["op_status"] != "in_flight" {
				t.Fatalf("scheduled message op_status = %v, want in_flight", r["op_status"])
			}
		}
	}
	if !found {
		t.Fatal("claimed scheduled op missing from the scheduled message projection")
	}
}

func TestCancelScheduledInFlightUnsealedDefersAndReturnsDraft(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	claimed := f.claimSendOp(t, opID)
	rec := f.deleteScheduled(t, opID, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel of a claimed-but-unsealed scheduled send status = %d, body %s; want a deferred acceptance, not a 409", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deferred bool `json:"deferred"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Deferred {
		t.Fatalf("cancel answer = %s, want deferred:true", rec.Body.String())
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpInFlight || !o.UndoRequested {
		t.Fatalf("op after the deferred cancel = (status %q, undo_requested %v), want in_flight with the undo flag set for the checkpoint", o.Status, o.UndoRequested)
	}
	// The executor reaches the checkpoint: the undo wins, the letter
	// returns to Drafts, nothing is delivered.
	if !f.srv.executeSend(f.srv.keys["tok"], claimed) {
		t.Fatal("executeSend did not land the deferred cancel")
	}
	if seams.deliverCalls != 0 {
		t.Fatal("a scheduled send cancelled before its checkpoint was delivered")
	}
	o, _ = f.st.GetOp(opID)
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
	if n := f.draftsCount(t); n != 1 {
		t.Fatalf("drafts after the deferred return-to-drafts = %d, want 1", n)
	}
	if _, err := os.Stat(outgoingDir(f.st.DataDir(), opID)); !os.IsNotExist(err) {
		t.Fatal("outgoing payload dir survived the deferred cancel")
	}
}

func TestPurgeScheduledInFlightUnsealedDefersAndDestroys(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	claimed := f.claimSendOp(t, opID)
	rec := f.deleteScheduled(t, opID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("purge of a claimed-but-unsealed scheduled send status = %d, body %s; want a deferred acceptance", rec.Code, rec.Body.String())
	}
	var resp struct {
		Deferred bool `json:"deferred"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Deferred {
		t.Fatalf("purge answer = %s, want deferred:true", rec.Body.String())
	}
	if !f.srv.executeSend(f.srv.keys["tok"], claimed) {
		t.Fatal("executeSend did not land the deferred purge")
	}
	if seams.deliverCalls != 0 {
		t.Fatal("a purged scheduled send was delivered")
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q, want cancelled", o.Status)
	}
	if n := f.draftsCount(t); n != 0 {
		t.Fatalf("drafts after the deferred purge = %d, want 0 (a true delete must not file the letter anywhere)", n)
	}
	if _, err := os.Stat(outgoingDir(f.st.DataDir(), opID)); !os.IsNotExist(err) {
		t.Fatal("outgoing payload dir survived the deferred purge; the letter still exists on disk")
	}
}

func TestCancelScheduledInFlightSealedIsHonestConflict(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	opID := f.scheduleSend(t, 0, time.Now().Add(2*time.Hour).Unix())
	claimed := f.claimSendOp(t, opID)
	sealed, _, err := f.st.SealSendForDelivery(opID, claimed.ClaimToken, f.srv.drainTokenSeq.Add(1))
	if err != nil || !sealed {
		t.Fatalf("seal = (%v, %v), want (true, nil)", sealed, err)
	}
	if rec := f.deleteScheduled(t, opID, false); rec.Code != http.StatusConflict {
		t.Fatalf("return-to-drafts of a sealed send status = %d, body %s; want an honest 409, the checkpoint is past", rec.Code, rec.Body.String())
	}
	if rec := f.deleteScheduled(t, opID, true); rec.Code != http.StatusConflict {
		t.Fatalf("purge of a sealed send status = %d, body %s; want an honest 409", rec.Code, rec.Body.String())
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpInFlight || !o.Sealed || o.UndoRequested {
		t.Fatalf("sealed op after the refused cancels = (status %q, sealed %v, undo_requested %v), want untouched in_flight sealed with no undo flag", o.Status, o.Sealed, o.UndoRequested)
	}
}
