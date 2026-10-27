package store

import (
	"path/filepath"
	"testing"
	"time"
)

func provUndoStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, kind, created_at) VALUES
		(2, 'p', 'p@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'p@x.test', '', 'proton', 0),
		(3, 't', 't@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 't@x.test', '', 'tuta', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(5, 2, 'INBOX', 'inbox'), (7, 2, 'Archive', 'archive'),
		(6, 3, 'INBOX', 'inbox'), (8, 3, 'Archive', 'archive'), (9, 3, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	return st
}

func seedTutaRow(t *testing.T, st *Store) {
	t.Helper()
	insertRow(t, st, 30, 3, 6, 77, "")
	if _, err := st.DB.Exec(`INSERT INTO tuta_uids(account_id, folder_name, uid, tuta_id) VALUES(3, 'INBOX', 77, 'T9')`); err != nil {
		t.Fatalf("tuta uids: %v", err)
	}
}

func seedProtonRow(t *testing.T, st *Store) {
	t.Helper()
	insertRow(t, st, 20, 2, 5, 55, "")
	if _, err := st.DB.Exec(`INSERT INTO proton_uids(account_id, folder_name, uid, proton_id) VALUES(2, 'INBOX', 55, 'P123')`); err != nil {
		t.Fatalf("proton uids: %v", err)
	}
}

func claimAndComplete(t *testing.T, st *Store, accountID, opID int64) {
	t.Helper()
	ops, err := st.ClaimDueOps(accountID, time.Now().Unix(), 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	found := false
	for _, o := range ops {
		if o.ID == opID {
			found = true
		}
	}
	if !found {
		t.Fatalf("claimed ops do not contain %d", opID)
	}
	if err := st.CompleteOp(opID); err != nil {
		t.Fatalf("complete op %d: %v", opID, err)
	}
}

func rowPlacement(t *testing.T, st *Store, rowID int64) (int64, int64) {
	t.Helper()
	var folderID, uid int64
	if err := st.DB.QueryRow("SELECT folder_id, uid FROM messages WHERE id = ?", rowID).Scan(&folderID, &uid); err != nil {
		t.Fatalf("row %d placement: %v", rowID, err)
	}
	return folderID, uid
}

func TestUndoDoneTutaMoveWithProvisionalRow(t *testing.T) {
	st := provUndoStore(t)
	seedTutaRow(t, st)
	opID, err := st.EnqueueMove(3, 30, 8, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Identity != "t:T9" {
		t.Fatalf("move identity = %q, want t:T9", o.Identity)
	}
	claimAndComplete(t, st, 3, opID)
	folderID, uid := rowPlacement(t, st, 30)
	if folderID != 8 || uid < provisionalUIDBase {
		t.Fatalf("row placement after completion = folder %d uid %d, want folder 8 with a provisional uid (the state this test exists for)", folderID, uid)
	}
	res, err := st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo of a completed move whose row is still provisional = %v, want success; the message exists, only its placement is provisional", err)
	}
	if res.Mode != "inverse" || res.Inverse == nil {
		t.Fatalf("undo result = %+v, want an inverse move op", res)
	}
	if res.Inverse.Identity != "t:T9" {
		t.Fatalf("inverse op identity = %q, want t:T9 (identity must survive the undo)", res.Inverse.Identity)
	}
	if folderID, _ := rowPlacement(t, st, 30); folderID != 6 {
		t.Fatalf("row folder after undo = %d, want 6 (INBOX)", folderID)
	}
	var undone int
	if err := st.DB.QueryRow("SELECT undone FROM ops WHERE id = ?", opID).Scan(&undone); err != nil {
		t.Fatalf("read undone for op %d: %v", opID, err)
	}
	if undone != 1 {
		t.Fatal("move op not marked undone")
	}
}

func TestUndoDoneProtonMoveWithProvisionalRow(t *testing.T) {
	st := provUndoStore(t)
	seedProtonRow(t, st)
	opID, err := st.EnqueueMove(2, 20, 7, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Identity != "p:P123" {
		t.Fatalf("move identity = %q, want p:P123", o.Identity)
	}
	claimAndComplete(t, st, 2, opID)
	folderID, uid := rowPlacement(t, st, 20)
	if folderID != 7 || uid < provisionalUIDBase {
		t.Fatalf("row placement after completion = folder %d uid %d, want folder 7 with a provisional uid", folderID, uid)
	}
	res, err := st.UndoOp(opID)
	if err != nil {
		t.Fatalf("undo of a completed move whose row is still provisional = %v, want success", err)
	}
	if res.Mode != "inverse" || res.Inverse == nil {
		t.Fatalf("undo result = %+v, want an inverse move op", res)
	}
	if res.Inverse.Identity != "p:P123" {
		t.Fatalf("inverse op identity = %q, want p:P123", res.Inverse.Identity)
	}
	if folderID, _ := rowPlacement(t, st, 20); folderID != 5 {
		t.Fatalf("row folder after undo = %d, want 5 (INBOX)", folderID)
	}
}

func TestUndoSecondOfChainedTutaMoves(t *testing.T) {
	st := provUndoStore(t)
	seedTutaRow(t, st)
	first, err := st.EnqueueMove(3, 30, 8, "")
	if err != nil {
		t.Fatalf("enqueue first move: %v", err)
	}
	if _, err := st.ClaimDueOps(3, time.Now().Unix(), 10); err != nil {
		t.Fatalf("claim first move: %v", err)
	}
	destUID, err := st.AssignTutaUID(3, "Archive", "T9")
	if err != nil {
		t.Fatalf("assign dest uid: %v", err)
	}
	if err := st.CompleteMoveOp(first, 8, 30, destUID); err != nil {
		t.Fatalf("complete first move: %v", err)
	}
	if folderID, uid := rowPlacement(t, st, 30); folderID != 8 || uid != int64(destUID) {
		t.Fatalf("row placement after first move = folder %d uid %d, want folder 8 uid %d", folderID, uid, destUID)
	}
	second, err := st.EnqueueMove(3, 30, 9, "")
	if err != nil {
		t.Fatalf("enqueue second move: %v", err)
	}
	so, err := st.GetOp(second)
	if err != nil {
		t.Fatalf("get second op: %v", err)
	}
	if so.Identity != "t:T9" {
		t.Fatalf("second move identity = %q, want t:T9", so.Identity)
	}
	claimAndComplete(t, st, 3, second)
	if folderID, uid := rowPlacement(t, st, 30); folderID != 9 || uid < provisionalUIDBase {
		t.Fatalf("row placement after second move = folder %d uid %d, want folder 9 with a provisional uid", folderID, uid)
	}
	res, err := st.UndoOp(second)
	if err != nil {
		t.Fatalf("undo of the second move = %v, want success; the first move aligned its placement, the second has not been re-listed yet", err)
	}
	if res.Inverse == nil || res.Inverse.Identity != "t:T9" {
		t.Fatalf("inverse op = %+v, want an inverse move carrying t:T9", res.Inverse)
	}
	if folderID, _ := rowPlacement(t, st, 30); folderID != 8 {
		t.Fatalf("row folder after undo = %d, want 8 (Archive, where the first move put it)", folderID)
	}
}

func TestUndoDoneFlagsAfterMoveToProvisionalRow(t *testing.T) {
	st := provUndoStore(t)
	seedTutaRow(t, st)
	seen := true
	flagsID, _, err := st.EnqueueFlags(3, 30, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	fo, err := st.GetOp(flagsID)
	if err != nil {
		t.Fatalf("get flags op: %v", err)
	}
	if fo.Identity != "t:T9" {
		t.Fatalf("flags identity = %q, want t:T9", fo.Identity)
	}
	moveID, err := st.EnqueueMove(3, 30, 8, "")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	if _, err := st.ClaimDueOps(3, time.Now().Unix(), 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.CompleteOp(flagsID); err != nil {
		t.Fatalf("complete flags: %v", err)
	}
	if err := st.CompleteOp(moveID); err != nil {
		t.Fatalf("complete move: %v", err)
	}
	res, err := st.UndoOp(flagsID)
	if err != nil {
		t.Fatalf("undo of completed flags on a moved message = %v, want success; the message only changed folders", err)
	}
	if res.Inverse == nil || res.Inverse.Type != OpFlags {
		t.Fatalf("inverse op = %+v, want an inverse flags op", res.Inverse)
	}
	if folderID, _ := rowPlacement(t, st, 30); folderID != 8 {
		t.Fatalf("row folder after flags undo = %d, want 8 (a flags undo must not move the message back)", folderID)
	}
}

func TestResolvePendingMoveSourceOnProvisionalRow(t *testing.T) {
	st := provUndoStore(t)
	seedTutaRow(t, st)
	first, err := st.EnqueueMove(3, 30, 8, "")
	if err != nil {
		t.Fatalf("enqueue first move: %v", err)
	}
	claimAndComplete(t, st, 3, first)
	second, err := st.EnqueueMove(3, 30, 9, "")
	if err != nil {
		t.Fatalf("enqueue second move: %v", err)
	}
	so, err := st.GetOp(second)
	if err != nil {
		t.Fatalf("get second op: %v", err)
	}
	if so.Identity != "t:T9" {
		t.Fatalf("second move identity = %q, want t:T9 even though its source row is provisional", so.Identity)
	}
	target, ok, err := st.ResolveMoveSource(so)
	if err != nil {
		t.Fatalf("resolve move source: %v", err)
	}
	if !ok || target.RowID != 30 {
		t.Fatalf("ResolveMoveSource = %+v, ok=%v; want the provisional row 30 as the source", target, ok)
	}
	if _, err := st.RefreshOpHints(3); err != nil {
		t.Fatalf("refresh hints over a pending move on a provisional row: %v", err)
	}
	cur, err := st.GetOp(second)
	if err != nil {
		t.Fatalf("get second op after refresh: %v", err)
	}
	if cur.MessageID != 30 {
		t.Fatalf("second op hint after refresh = row %d, want 30", cur.MessageID)
	}
}
