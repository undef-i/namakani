package store

import (
	"errors"
	"testing"
	"time"
)

func TestReleaseInFlightOpsLeavesSealedClaimsAlone(t *testing.T) {
	st := identityTestStore(t)
	sealedOp := claimSendForSeal(t, st)
	if _, _, err := st.SealSendForDelivery(sealedOp.ID, 42, 43); err != nil {
		t.Fatalf("seal: %v", err)
	}
	plainID, err := st.EnqueueSend(1, `{}`, 0, "")
	if err != nil {
		t.Fatalf("enqueue second send: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'in_flight', claim_token = 77 WHERE id = ?", plainID); err != nil {
		t.Fatalf("strand second send: %v", err)
	}
	if _, err := st.ReleaseInFlightOps(1); err != nil {
		t.Fatalf("release: %v", err)
	}
	cur, err := st.GetOp(sealedOp.ID)
	if err != nil {
		t.Fatalf("get sealed op: %v", err)
	}
	if cur.Status != OpInFlight || !cur.Sealed {
		t.Fatalf("sealed op after release = (status %q, sealed %v), want (in_flight, true): the reaper reopened the checkpoint", cur.Status, cur.Sealed)
	}
	plain, err := st.GetOp(plainID)
	if err != nil {
		t.Fatalf("get plain op: %v", err)
	}
	if plain.Status != OpPending || plain.ClaimToken != 0 {
		t.Fatalf("unsealed op after release = (status %q, token %d), want (pending, 0)", plain.Status, plain.ClaimToken)
	}
}

func TestAdoptSealedSendRotatesTokenOnce(t *testing.T) {
	st := identityTestStore(t)
	o := claimSendForSeal(t, st)
	if _, _, err := st.SealSendForDelivery(o.ID, 42, 43); err != nil {
		t.Fatalf("seal: %v", err)
	}
	orphans, err := st.ListSealedInFlightSends(1)
	if err != nil || len(orphans) != 1 || orphans[0].ID != o.ID {
		t.Fatalf("sealed orphans = %+v, %v; want exactly the sealed op", orphans, err)
	}
	ok, err := st.AdoptSealedSend(o.ID, 999, 100)
	if err != nil || ok {
		t.Fatalf("adopt under a foreign token = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = st.AdoptSealedSend(o.ID, 43, 100)
	if err != nil || !ok {
		t.Fatalf("adopt under the seal token = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = st.AdoptSealedSend(o.ID, 43, 101)
	if err != nil || ok {
		t.Fatalf("second adopt under the pre-adoption token = (%v, %v), want (false, nil): exactly one settler may take a sealed op", ok, err)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.ClaimToken != 100 || cur.Status != OpInFlight || !cur.Sealed {
		t.Fatalf("adopted op = (token %d, status %q, sealed %v), want (100, in_flight, true)", cur.ClaimToken, cur.Status, cur.Sealed)
	}
}

func TestParkSealedSendUnknownIsTerminalForUndoAndRetry(t *testing.T) {
	st := identityTestStore(t)
	o := claimSendForSeal(t, st)
	if _, _, err := st.SealSendForDelivery(o.ID, 42, 43); err != nil {
		t.Fatalf("seal: %v", err)
	}
	ok, err := st.ParkSealedSendUnknown(o.ID)
	if err != nil || !ok {
		t.Fatalf("park sealed op = (%v, %v), want (true, nil)", ok, err)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if cur.Status != OpNeedsAttention || cur.LastError != SendOutcomeUnknownError {
		t.Fatalf("parked op = (status %q, error %q), want needs_attention with the outcome-unknown marker", cur.Status, cur.LastError)
	}
	res, err := st.UndoOp(o.ID)
	if err != nil {
		t.Fatalf("undo of a parked send: %v", err)
	}
	if res.Mode != "sealed" {
		t.Fatalf("undo of a parked send mode = %q, want sealed: delivery is unproven either way, the books must not claim it was cancelled", res.Mode)
	}
	cur, _ = st.GetOp(o.ID)
	if cur.Status != OpNeedsAttention {
		t.Fatalf("op status after the sealed undo answer = %q, want needs_attention (unchanged)", cur.Status)
	}
	if err := st.RetryOp(o.ID); !errors.Is(err, ErrOpNotActive) {
		t.Fatalf("retry of a parked send err = %v, want ErrOpNotActive: redriving an unproven delivery is how duplicates happen", err)
	}
	cur, _ = st.GetOp(o.ID)
	if cur.Status != OpNeedsAttention {
		t.Fatalf("op status after the refused retry = %q, want needs_attention", cur.Status)
	}
}

func TestUndoSealedSendBackInPendingStillCancels(t *testing.T) {
	st := identityTestStore(t)
	o := claimSendForSeal(t, st)
	if _, _, err := st.SealSendForDelivery(o.ID, 42, 43); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := st.FailOp(o.ID, "provider refused", false, time.Now().Unix()); err != nil {
		t.Fatalf("fail op: %v", err)
	}
	cur, err := st.GetOp(o.ID)
	if err != nil || cur.Status != OpPending || !cur.Sealed {
		t.Fatalf("op after failed delivery = (%+v, %v), want pending and still sealed", cur, err)
	}
	res, err := st.UndoOp(o.ID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if res.Mode != "cancelled" {
		t.Fatalf("undo of a sealed send whose delivery provably failed mode = %q, want cancelled", res.Mode)
	}
	cur, _ = st.GetOp(o.ID)
	if cur.Status != OpCancelled {
		t.Fatalf("op status = %q, want cancelled", cur.Status)
	}
}
