package store

import "testing"

func TestUndoDoneSendReportsAlreadySent(t *testing.T) {
	st := opsTestStore(t)
	id, err := st.EnqueueSend(1, "{}", 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	if _, err := st.DB.Exec("UPDATE ops SET status = 'done' WHERE id = ?", id); err != nil {
		t.Fatalf("force done: %v", err)
	}
	res, err := st.UndoOp(id)
	if err != nil {
		t.Fatalf("undo of a done send must answer already_sent, not fail: %v", err)
	}
	if res.Mode != "already_sent" {
		t.Fatalf("undo mode = %q, want already_sent", res.Mode)
	}
	if res.Inverse != nil {
		t.Fatal("undoing a delivered send created an inverse op; that would delete the filed copy and fake an undo")
	}
	o, err := st.GetOp(id)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Status != OpDone {
		t.Fatalf("op status = %q, want done (nothing about a delivered send changes)", o.Status)
	}
	var undone, total int
	_ = st.DB.QueryRow("SELECT undone FROM ops WHERE id = ?", id).Scan(&undone)
	_ = st.DB.QueryRow("SELECT COUNT(*) FROM ops").Scan(&total)
	if undone != 0 {
		t.Fatal("the op was marked undone even though the send was not undone")
	}
	if total != 1 {
		t.Fatalf("ops = %d, want 1", total)
	}
}
