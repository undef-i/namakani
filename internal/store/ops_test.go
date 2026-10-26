package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEnqueueFlagsDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'INBOX', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags) VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	opID, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if opID == 0 {
		t.Fatal("enqueue returned no op")
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'in_flight' WHERE id = ?", opID); err != nil {
		t.Fatalf("force in_flight: %v", err)
	}
	st.DB.Close()
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.DB.Close()
	o, err := st2.GetOp(opID)
	if err != nil {
		t.Fatalf("get op after reopen: %v", err)
	}
	if o.Status != OpPending {
		t.Fatalf("op status after reopen = %q, want pending (in-flight must reset on boot)", o.Status)
	}
	flags, _ := st2.MessageFlags(10)
	if flags&1 == 0 {
		t.Fatal("optimistic mirror flags lost across reopen")
	}
}

func TestEnqueueFlagsCoalesceBackToServerState(t *testing.T) {
	st := opsTestStore(t)
	opID, flags, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil || opID == 0 {
		t.Fatalf("first enqueue: op=%d flags=%d err=%v", opID, flags, err)
	}
	opID2, flags, err := st.EnqueueFlags(1, 10, boolPtr(false), nil, "")
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if opID2 != 0 {
		t.Fatalf("toggling back to server state returned op %d, want the op to dissolve", opID2)
	}
	if flags&1 != 0 {
		t.Fatalf("mirror flags = %d, want unread restored", flags)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != OpCancelled {
		t.Fatalf("dissolved op status = %q, want cancelled", o.Status)
	}
	due, err := st.ClaimDueOps(1, time.Now().Unix()+100000, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("claimed %d ops, want 0 (the gesture cancelled itself out)", len(due))
	}
}

func TestClaimOrderAndHeadOfLine(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	moveID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	flagsID, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	otherID, _, err := st.EnqueueFlags(1, 11, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue other flags: %v", err)
	}
	due, err := st.ClaimDueOps(1, now+10, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := map[int64]bool{}
	for _, o := range due {
		got[o.ID] = true
	}
	if !got[moveID] || !got[otherID] {
		t.Fatalf("claimed %v, want the move and the other message's flags op", got)
	}
	if got[flagsID] {
		t.Fatal("flags op behind a pending move for the same message must not be claimed yet")
	}
	if err := st.CompleteOp(moveID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	due, err = st.ClaimDueOps(1, now+10, 10)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if len(due) != 1 || due[0].ID != flagsID {
		t.Fatalf("after the move drained, claimed %v, want only the flags op", due)
	}
}

func TestClaimRespectsNotBeforeAndBackoff(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	opID, err := st.EnqueueSend(1, "{}", now+3600, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	due, err := st.ClaimDueOps(1, now+10, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 0 {
		t.Fatal("op inside its not_before window must not be claimed")
	}
	due, err = st.ClaimDueOps(1, now+3601, 10)
	if err != nil {
		t.Fatalf("claim after window: %v", err)
	}
	if len(due) != 1 || due[0].ID != opID {
		t.Fatalf("after window claimed %v, want the send op", due)
	}
	status, err := st.FailOp(opID, "dial timeout", false, now+3601)
	if err != nil {
		t.Fatalf("fail op: %v", err)
	}
	if status != OpPending {
		t.Fatalf("transient failure status = %q, want pending (retry scheduled)", status)
	}
	o, _ := st.GetOp(opID)
	if o.NextAttempt <= now+3601 {
		t.Fatalf("next_attempt = %d, want a future backoff time", o.NextAttempt)
	}
	due, err = st.ClaimDueOps(1, now+3602, 10)
	if err != nil {
		t.Fatalf("claim during backoff: %v", err)
	}
	if len(due) != 0 {
		t.Fatal("op in backoff must not be claimed")
	}
}

func TestFailOpParksAfterMaxAttempts(t *testing.T) {
	st := opsTestStore(t)
	now := time.Now().Unix()
	opID, _, err := st.EnqueueFlags(1, 10, boolPtr(true), nil, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var status string
	for i := 0; i < maxOpAttempts; i++ {
		if _, err := st.ClaimDueOps(1, now+1000000, 10); err != nil {
			t.Fatalf("claim: %v", err)
		}
		status, err = st.FailOp(opID, "upstream 500", false, now)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
	}
	if status != OpNeedsAttention {
		t.Fatalf("status after %d transient failures = %q, want needs_attention", maxOpAttempts, status)
	}
	flags, _ := st.MessageFlags(10)
	if flags&1 == 0 {
		t.Fatal("parking an op must not roll back the user's visible state")
	}
	if err := st.RetryOp(opID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	o, _ := st.GetOp(opID)
	if o.Status != OpPending || o.Attempts != 0 {
		t.Fatalf("after retry: status=%q attempts=%d, want pending/0", o.Status, o.Attempts)
	}
}

func TestCancelPendingMoveRestoresExactly(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.DB.Exec("UPDATE folders SET total = 2 WHERE id = 1"); err != nil {
		t.Fatalf("seed totals: %v", err)
	}
	opID, err := st.EnqueueMove(1, 10, 2, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	var midTotal1, midTotal2 int
	_ = st.DB.QueryRow("SELECT total FROM folders WHERE id = 1").Scan(&midTotal1)
	_ = st.DB.QueryRow("SELECT total FROM folders WHERE id = 2").Scan(&midTotal2)
	if midTotal1 != 1 || midTotal2 != 1 {
		t.Fatalf("folder totals after enqueue = %d/%d, want 1/1", midTotal1, midTotal2)
	}
	res, err := st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if res.Mode != "cancelled" {
		t.Fatalf("undo mode = %q, want cancelled", res.Mode)
	}
	route, err := st.MessageRoute(10)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.FolderID != 1 || route.UID != 100 {
		t.Fatalf("route after undo = folder %d uid %d, want folder 1 uid 100 (the exact prior location)", route.FolderID, route.UID)
	}
	var total1, total2 int
	_ = st.DB.QueryRow("SELECT total FROM folders WHERE id = 1").Scan(&total1)
	_ = st.DB.QueryRow("SELECT total FROM folders WHERE id = 2").Scan(&total2)
	if total1 != 2 || total2 != 0 {
		t.Fatalf("folder totals after undo = %d/%d, want the exact prior state 2/0", total1, total2)
	}
}

func TestDoubleExpungeCoalesces(t *testing.T) {
	st := opsTestStore(t)
	first, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("first expunge: %v", err)
	}
	second, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("second expunge: %v", err)
	}
	if first == 0 || second != first {
		t.Fatalf("double expunge gave ops %d and %d, want one shared op", first, second)
	}
	hidden := st.PendingExpungeSet([]int64{10})
	if !hidden[10] {
		t.Fatal("message with a pending expunge must read as hidden")
	}
	res, err := st.UndoOp(first)
	if err != nil || res.Mode != "cancelled" {
		t.Fatalf("undo expunge: res=%v err=%v", res, err)
	}
	if st.PendingExpungeSet([]int64{10})[10] {
		t.Fatal("cancelling the expunge must make the message visible again")
	}
}
