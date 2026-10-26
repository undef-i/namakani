package api

import (
	"testing"

	"namakani/internal/store"
)

func TestDrainRecoversOpStrandedInFlight(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postFlags(t, 10, `{"seen":true}`)
	opID := opIDOf(t, rec)
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", opID); err != nil {
		t.Fatalf("strand op in_flight: %v", err)
	}
	f.srv.drainAccount(nil, 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("stranded op status after drain = %q, want done (its wave ended without landing it; the next drain must pick it up, not skip it forever)", o.Status)
	}
	if n := len(f.p.callsOf("StoreFlags")); n != 1 {
		t.Fatalf("provider StoreFlags calls = %d, want 1 (the recovered op must actually execute)", n)
	}
}
