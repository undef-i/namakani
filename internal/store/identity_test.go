package store

import (
	"path/filepath"
	"testing"
)

func identityTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, kind, created_at) VALUES
		(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 'imap', 0),
		(2, 'p', 'p@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'p@x.test', '', 'proton', 0),
		(3, 't', 't@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 't@x.test', '', 'tuta', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Archive', 'archive'), (3, 1, 'Trash', 'trash'), (4, 1, 'All Mail', 'all'),
		(5, 2, 'INBOX', 'inbox'), (6, 3, 'INBOX', 'inbox')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	return st
}

func insertRow(t *testing.T, st *Store, id, accountID, folderID int64, uid uint32, mid string) {
	t.Helper()
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject)
		VALUES(?, ?, ?, ?, ?, 's')`, id, accountID, folderID, uid, mid); err != nil {
		t.Fatalf("insert row %d: %v", id, err)
	}
}

func TestIdentityCaptureForms(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue expunge: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.Identity != "m:m10@x" {
		t.Fatalf("imap identity = %q, want m:m10@x", o.Identity)
	}

	insertRow(t, st, 11, 1, 1, 101, "")
	seen := true
	fID, _, err := st.EnqueueFlags(1, 11, &seen, nil, "")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	fo, err := st.GetOp(fID)
	if err != nil {
		t.Fatalf("get flags op: %v", err)
	}
	direct, err := st.IdentityForRow(1, 11)
	if err != nil || direct != fo.Identity {
		t.Fatalf("IdentityForRow = %q, %v; want %q", direct, err, fo.Identity)
	}
	if fo.PriorFolderID != 1 {
		t.Fatalf("flags op prior_folder_id = %d, want 1 (anchor folder recorded)", fo.PriorFolderID)
	}

	insertRow(t, st, 20, 2, 5, 55, "ignored@x")
	if _, err := st.DB.Exec(`INSERT INTO proton_uids(account_id, folder_name, uid, proton_id) VALUES(2, 'INBOX', 55, 'P123')`); err != nil {
		t.Fatalf("proton uids: %v", err)
	}
	pID, err := st.EnqueueExpunge(2, 20, "")
	if err != nil {
		t.Fatalf("enqueue proton expunge: %v", err)
	}
	po, err := st.GetOp(pID)
	if err != nil {
		t.Fatalf("get proton op: %v", err)
	}
	if po.Identity != "p:P123" {
		t.Fatalf("proton identity = %q, want p:P123 (provider id outranks Message-ID)", po.Identity)
	}

	insertRow(t, st, 30, 3, 6, 77, "")
	if _, err := st.DB.Exec(`INSERT INTO tuta_uids(account_id, folder_name, uid, tuta_id) VALUES(3, 'INBOX', 77, 'T9')`); err != nil {
		t.Fatalf("tuta uids: %v", err)
	}
	tID, err := st.EnqueueExpunge(3, 30, "")
	if err != nil {
		t.Fatalf("enqueue tuta expunge: %v", err)
	}
	to, err := st.GetOp(tID)
	if err != nil {
		t.Fatalf("get tuta op: %v", err)
	}
	if to.Identity != "t:T9" {
		t.Fatalf("tuta identity = %q, want t:T9", to.Identity)
	}
}

func TestResolveAfterWipeRebuild(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	target, ok, err := st.ResolveOpTarget(o)
	if err != nil || !ok {
		t.Fatalf("resolve = %v, %v; want resolved", ok, err)
	}
	if target.RowID != 77 || target.UID != 140 || target.FolderID != 1 {
		t.Fatalf("target = %+v, want row 77 uid 140 folder 1", target)
	}
	after, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op after: %v", err)
	}
	if after.MessageID != 77 {
		t.Fatalf("hint after resolve = %d, want 77 (cache coordinate refreshed)", after.MessageID)
	}
	if after.Status != OpPending {
		t.Fatalf("status after resolve = %q, want pending (resolution never kills)", after.Status)
	}
}

func TestResolveUIDReuseByStranger(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "mine@x")
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 78, 1, 1, 100, "stranger@x")
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if _, ok, err := st.ResolveOpTarget(o); err != nil || ok {
		t.Fatalf("resolve with only a stranger at the old uid = %v, %v; want unresolved", ok, err)
	}
	insertRow(t, st, 79, 1, 1, 140, "mine@x")
	target, ok, err := st.ResolveOpTarget(o)
	if err != nil || !ok {
		t.Fatalf("resolve after the real message returns = %v, %v", ok, err)
	}
	if target.RowID != 79 || target.UID != 140 {
		t.Fatalf("target = %+v, want the identity's own row 79 at uid 140, never the stranger at uid 100", target)
	}
}

func TestResolveAmbiguityRules(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "dup@x")
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	// Copies in two other folders, none in the anchor folder: no safe pick.
	insertRow(t, st, 80, 1, 2, 200, "dup@x")
	insertRow(t, st, 81, 1, 4, 300, "dup@x")
	if _, ok, err := st.ResolveOpTarget(o); err != nil || ok {
		t.Fatalf("resolve across two foreign copies = %v, %v; want unresolved (no guessing)", ok, err)
	}
	// The anchor-folder incarnation returns: it is the copy the user acted on.
	insertRow(t, st, 82, 1, 1, 150, "dup@x")
	target, ok, err := st.ResolveOpTarget(o)
	if err != nil || !ok || target.RowID != 82 {
		t.Fatalf("resolve with anchor copy present = %+v, %v, %v; want row 82", target, ok, err)
	}
	// A second copy inside the anchor folder makes it ambiguous again.
	insertRow(t, st, 83, 1, 1, 151, "dup@x")
	if _, ok, err := st.ResolveOpTarget(o); err != nil || ok {
		t.Fatalf("resolve with two anchor-folder copies = %v, %v; want unresolved", ok, err)
	}
}

func TestRefreshOpHintsFollowsRebuild(t *testing.T) {
	st := identityTestStore(t)
	insertRow(t, st, 10, 1, 1, 100, "m10@x")
	opID, err := st.EnqueueExpunge(1, 10, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	insertRow(t, st, 77, 1, 1, 140, "m10@x")
	n, err := st.RefreshOpHints(1)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n != 1 {
		t.Fatalf("refreshed = %d, want 1", n)
	}
	o, err := st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if o.MessageID != 77 || o.Status != OpPending {
		t.Fatalf("op after refresh = row %d status %q, want row 77 pending", o.MessageID, o.Status)
	}
}

