package api

import (
	"testing"

	"namakani/internal/store"
)

func TestMoveWithoutCopyUIDClaimsLocalTwinRow(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	f.p.seed("INBOX", 100, 0)
	f.p.seed("Archive", 555, 0)
	f.p.seed("Archive", 556, 0)
	f.p.moveNoUID = true
	opID, err := f.st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(99, 1, 2, 555, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert twin row: %v", err)
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s (last error %q), want done: the target folder already holds this message under a real uid, the op must claim that row instead of failing", o.Status, o.LastError)
	}
	var folderID int64
	var uid uint32
	if err := f.st.DB.QueryRow("SELECT folder_id, uid FROM messages WHERE id = 10").Scan(&folderID, &uid); err != nil {
		t.Fatalf("read moved row: %v", err)
	}
	if folderID != 2 || uid != 555 {
		t.Fatalf("moved row = folder %d uid %d, want folder 2 uid 555", folderID, uid)
	}
	var twins int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 99").Scan(&twins); err != nil {
		t.Fatalf("count twin: %v", err)
	}
	if twins != 0 {
		t.Fatalf("twin row 99 still present: the claimed row must replace it, not duplicate it")
	}
}
