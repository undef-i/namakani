package api

import (
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

func (f *drainFixture) setValidity(t *testing.T, folderID int64, v uint32) {
	t.Helper()
	if _, err := f.st.DB.Exec("UPDATE folders SET uidvalidity = ? WHERE id = ?", v, folderID); err != nil {
		t.Fatalf("set validity: %v", err)
	}
}

// rebuild replaces a folder's local rows the way a UIDVALIDITY rebuild
// does: wipe, then the same messages return under new row ids and new uids.
func (f *drainFixture) rebuildRow(t *testing.T, folderID, newRowID int64, uid uint32, mid, folderName string) {
	t.Helper()
	if err := f.st.WipeFolder(folderID); err != nil {
		t.Fatalf("wipe folder %d: %v", folderID, err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(?, 1, ?, ?, ?, 0)`, newRowID, folderID, uid, mid); err != nil {
		t.Fatalf("reinsert row %d: %v", newRowID, err)
	}
	f.p.seed(folderName, uid, 0)
}

func TestRebuiltFolderMoveExecutesAgainstNewIncarnation(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	opID, err := f.st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	// The destination rebuild kills the enqueue-time phantom row; the
	// source rebuild brings the true message back under a new row and uid.
	if err := f.st.WipeFolder(2); err != nil {
		t.Fatalf("wipe dest: %v", err)
	}
	f.rebuildRow(t, 1, 77, 140, "m10@x", "INBOX")
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	calls := f.p.callsOf("MoveMessage")
	if len(calls) != 1 || len(calls[0].UIDs) != 1 || calls[0].UIDs[0] != 140 || calls[0].Dest != "Archive" {
		t.Fatalf("MoveMessage calls = %+v, want exactly one move of the new incarnation uid 140 to Archive", calls)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done", o.Status)
	}
	route, err := f.st.MessageRoute(77)
	if err != nil {
		t.Fatalf("route rebuilt row: %v", err)
	}
	if route.FolderID != 2 {
		t.Fatalf("rebuilt row landed in folder %d, want 2 (Archive) after the move completed", route.FolderID)
	}
}

func TestRebuiltFolderExpungeDeletesNewIncarnation(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 11, 3, "Trash", 101, 0)
	opID, err := f.st.EnqueueExpunge(1, 11, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	f.rebuildRow(t, 3, 78, 150, "m11@x", "Trash")
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	calls := f.p.callsOf("DeleteMessage")
	if len(calls) != 1 || len(calls[0].UIDs) != 1 || calls[0].UIDs[0] != 150 {
		t.Fatalf("DeleteMessage calls = %+v, want exactly one delete of the new incarnation uid 150", calls)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done", o.Status)
	}
	if _, err := f.st.MessageRoute(78); err == nil {
		t.Fatal("rebuilt row still present after expunge; want it deleted")
	}
}

func TestRebuiltFolderFlagsApplyToNewIncarnation(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 12, 1, "INBOX", 102, 0)
	seen := true
	opID, _, err := f.st.EnqueueFlags(1, 12, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	f.rebuildRow(t, 1, 79, 141, "m12@x", "INBOX")
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	found := false
	for _, c := range f.p.callsOf("StoreFlags") {
		for _, u := range c.UIDs {
			if u == 141 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("StoreFlags calls = %+v, want the seen flag applied to the new incarnation uid 141", f.p.callsOf("StoreFlags"))
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done", o.Status)
	}
}

func TestRebuiltFolderUIDReuseByStrangerUntouched(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 13, 3, "Trash", 103, 0)
	opID, err := f.st.EnqueueExpunge(1, 13, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if err := f.st.WipeFolder(3); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	// A stranger's message now sits at the old uid; the target never returned.
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(80, 1, 3, 103, 'stranger@x', 0)`); err != nil {
		t.Fatalf("insert stranger: %v", err)
	}
	f.p.seed("Trash", 103, 0)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	if n := len(f.p.callsOf("DeleteMessage")); n != 0 {
		t.Fatalf("DeleteMessage calls = %d, want 0: a reused uid must never be acted on for another message's op", n)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done (landed as gone)", o.Status)
	}
	if _, err := f.st.MessageRoute(80); err != nil {
		t.Fatal("the stranger's row was removed; identity-unresolved ops must not touch rows")
	}
}

func TestEpochChangeAloneDoesNotKillOp(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 14, 1, "INBOX", 104, 0)
	f.setValidity(t, 1, 7)
	opID, err := f.st.EnqueueMove(1, 14, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	f.setValidity(t, 1, 9)
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	calls := f.p.callsOf("MoveMessage")
	if len(calls) != 1 || len(calls[0].UIDs) != 1 || calls[0].UIDs[0] != 104 {
		t.Fatalf("MoveMessage calls = %+v, want the move executed: an epoch counter change alone, with the message untouched, is not a death sentence", calls)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done", o.Status)
	}
}

// The production incident shape: a parked expunge whose folder was rebuilt
// underneath it must, on retry, delete the message's new incarnation.
func TestParkedExpungeRetryDeletesRebuiltIncarnation(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	f.addMessage(t, 15, 3, "Trash", 105, 0)
	opID, err := f.st.EnqueueExpunge(1, 15, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET status = 'needs_attention', attempts = 8 WHERE id = ?", opID); err != nil {
		t.Fatalf("park op: %v", err)
	}
	f.rebuildRow(t, 3, 81, 160, "m15@x", "Trash")
	if err := f.st.RetryOp(opID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	f.srv.drainAccount(f.srv.keys["tok"], 1)
	calls := f.p.callsOf("DeleteMessage")
	if len(calls) != 1 || len(calls[0].UIDs) != 1 || calls[0].UIDs[0] != 160 {
		t.Fatalf("DeleteMessage calls = %+v, want the retried expunge to delete the rebuilt incarnation uid 160", calls)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %s, want done", o.Status)
	}
}

type probeConn struct {
	validity uint32
	uids     []uint32
}

func (c *probeConn) Close() {}
func (c *probeConn) Capabilities() []string {
	return nil
}
func (c *probeConn) HasCap(cap imap.Cap) bool { return false }
func (c *probeConn) ListFolders() ([]mail.FolderInfo, error) {
	return nil, nil
}
func (c *probeConn) Select(name string) (*mail.SelectData, error) {
	return &mail.SelectData{UIDValidity: c.validity, UIDNext: 2000}, nil
}
func (c *probeConn) SelectRW(name string) (*mail.SelectData, error) { return c.Select(name) }
func (c *probeConn) FetchUIDs() ([]uint32, error)                   { return c.uids, nil }
func (c *probeConn) FetchEnvelopes(uids []uint32) ([]mail.Msg, error) {
	return nil, nil
}
func (c *probeConn) FetchFlags(uint64) ([]mail.Msg, error) { return nil, nil }
func (c *probeConn) FetchRaw(uid uint32) ([]byte, error)   { return nil, nil }
func (c *probeConn) FetchThreadHeaders(uids []uint32) (map[uint32]mail.ThreadHeaders, error) {
	return nil, nil
}
func (c *probeConn) StoreFlag(uid uint32, flag imap.Flag, add bool) error    { return nil }
func (c *probeConn) StoreFlags(uids []uint32, flag imap.Flag, add bool) error { return nil }
func (c *probeConn) MoveMessage(uid uint32, dest string) (uint32, error)     { return 0, nil }
func (c *probeConn) DeleteMessage(uid uint32) error                          { return nil }
func (c *probeConn) AppendMessage(folder string, raw []byte, flags ...imap.Flag) (uint32, error) {
	return 0, nil
}
