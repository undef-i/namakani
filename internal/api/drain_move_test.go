package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"namakani/internal/store"
)

func (f *drainFixture) postMove(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/messages/%d/move", id), strings.NewReader(body))
	req.SetPathValue("id", fmt.Sprint(id))
	rec := httptest.NewRecorder()
	f.srv.handleMoveMessage(rec, req)
	return rec
}

func (f *drainFixture) postDelete(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/messages/%d/delete", id), strings.NewReader(body))
	req.SetPathValue("id", fmt.Sprint(id))
	rec := httptest.NewRecorder()
	f.srv.handleDeleteMessage(rec, req)
	return rec
}

func (f *drainFixture) postUndo(t *testing.T, opID int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/ops/%d/undo", opID), nil)
	req.SetPathValue("id", fmt.Sprint(opID))
	rec := httptest.NewRecorder()
	f.srv.handleUndoOp(rec, req)
	return rec
}

func opIDOf(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var resp struct {
		OpID int64 `json:"op_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode op response: %v (body %s)", err, rec.Body.String())
	}
	return resp.OpID
}

func TestMoveHandlerEnqueuesThenDrains(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("move handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)
	if opID == 0 {
		t.Fatal("move handler returned no op_id")
	}
	route, err := f.st.MessageRoute(10)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.FolderID != 2 {
		t.Fatalf("mirror folder right after the request = %d, want 2 (Archive) before any drain", route.FolderID)
	}
	if _, ok := f.p.flags("INBOX", 100); !ok {
		t.Fatal("provider lost the message before the drain ran")
	}
	if _, ok := f.p.flags("Archive", 100); ok {
		t.Fatal("provider already has the message in Archive before the drain ran")
	}
	f.srv.drainAccount(nil, 1)
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpDone {
		t.Fatalf("op status after drain = %q, want done", o.Status)
	}
	if _, ok := f.p.flags("INBOX", 100); ok {
		t.Fatal("provider still has the message in INBOX after drain")
	}
	route, _ = f.st.MessageRoute(10)
	pf, ok := f.p.flags("Archive", route.UID)
	if !ok {
		t.Fatalf("provider Archive has no message at the mirror's uid %d after drain", route.UID)
	}
	_ = pf
	if route.UID >= 1<<31 {
		t.Fatalf("mirror uid after drain = %d, still provisional; the real provider uid must be stamped", route.UID)
	}
	if n := len(f.p.callsOf("MoveMessage")); n != 1 {
		t.Fatalf("provider MoveMessage calls = %d, want 1", n)
	}
}

func TestUndoMoveBeforeDrainCancelsExactly(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	rec = f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Mode != "cancelled" {
		t.Fatalf("undo mode = %q, want cancelled (body %s)", resp.Mode, rec.Body.String())
	}
	route, _ := f.st.MessageRoute(10)
	if route.FolderID != 1 || route.UID != 100 {
		t.Fatalf("route after undo = folder %d uid %d, want the exact prior location 1/100", route.FolderID, route.UID)
	}
	f.srv.drainAccount(nil, 1)
	if n := len(f.p.callsOf("MoveMessage")); n != 0 {
		t.Fatalf("provider MoveMessage calls after a pre-drain undo = %d, want 0", n)
	}
	if _, ok := f.p.flags("INBOX", 100); !ok {
		t.Fatal("provider message vanished after a cancelled move")
	}
}

func TestUndoMoveAfterDrainCreatesInverse(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	movedRoute, _ := f.st.MessageRoute(10)
	if movedRoute.FolderID != 2 {
		t.Fatalf("setup: message not in Archive after drain")
	}
	rec = f.postUndo(t, opID)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Mode != "inverse" {
		t.Fatalf("undo mode = %q, want inverse once the op has drained", resp.Mode)
	}
	route, _ := f.st.MessageRoute(10)
	if route.FolderID != 1 {
		t.Fatalf("mirror folder after undo = %d, want 1 (INBOX)", route.FolderID)
	}
	f.srv.drainAccount(nil, 1)
	calls := f.p.callsOf("MoveMessage")
	if len(calls) != 2 {
		t.Fatalf("provider MoveMessage calls = %d, want 2 (move out and move back)", len(calls))
	}
	back := calls[1]
	if back.Folder != "Archive" || back.Dest != "INBOX" || back.UIDs[0] != movedRoute.UID {
		t.Fatalf("inverse move addressed %+v, want source Archive uid %d dest INBOX", back, movedRoute.UID)
	}
	route, _ = f.st.MessageRoute(10)
	if _, ok := f.p.flags("INBOX", route.UID); !ok {
		t.Fatal("provider does not hold the message in INBOX at the mirror uid after the inverse drained")
	}
}

func TestDeleteHandlerTrashThenExpunge(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postDelete(t, 10, `{}`)
	opID := opIDOf(t, rec)
	o, _ := f.st.GetOp(opID)
	if o.Type != store.OpMove || o.ToFolderID != 3 {
		t.Fatalf("delete from INBOX created op type=%q to=%d, want a move to Trash (3)", o.Type, o.ToFolderID)
	}
	f.srv.drainAccount(nil, 1)
	route, _ := f.st.MessageRoute(10)
	if route.FolderID != 3 {
		t.Fatalf("after delete+drain the mirror folder = %d, want Trash (3)", route.FolderID)
	}
	rec = f.postDelete(t, 10, `{}`)
	opID = opIDOf(t, rec)
	o, _ = f.st.GetOp(opID)
	if o.Type != store.OpExpunge {
		t.Fatalf("delete from Trash created op type=%q, want expunge", o.Type)
	}
	if !f.st.PendingExpungeSet([]int64{10})[10] {
		t.Fatal("message is not hidden while its expunge is pending")
	}
	f.srv.drainAccount(nil, 1)
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("local row survived a drained expunge")
	}
	if n := len(f.p.callsOf("DeleteMessage")); n != 1 {
		t.Fatalf("provider DeleteMessage calls = %d, want 1", n)
	}
}

func TestDeleteSameGestureKeyCoalesces(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postDelete(t, 10, `{"client_key":"gesture-1"}`)
	first := opIDOf(t, rec)
	rec = f.postDelete(t, 10, `{"client_key":"gesture-1"}`)
	second := opIDOf(t, rec)
	if second != first {
		t.Fatalf("a retried request with the same client key created op %d after op %d; want the same op", second, first)
	}
	var n int
	_ = f.st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE message_id = 10 AND status IN ('pending','in_flight','needs_attention')").Scan(&n)
	if n != 1 {
		t.Fatalf("active ops for the message = %d, want exactly 1", n)
	}
}

func TestMoveGoneWithoutProbeParksInsteadOfDeletingRow(t *testing.T) {
	f := newDrainFixture(t)
	if _, err := f.st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, flags)
		VALUES(10, 1, 1, 100, 'm10@x', 0)`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	f.p.failSelectOnce = map[string]int{"Archive": 1}
	rec := f.postMove(t, 10, `{"to":"archive"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("move handler status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)
	f.srv.drainAccount(nil, 1)
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 10").Scan(&n); err != nil {
		t.Fatalf("count message: %v", err)
	}
	if n != 1 {
		t.Fatalf("local row was deleted although the move target state was never proven (probe select failed and the source reported gone); the op must park for review instead of dropping the row and its cached body")
	}
	o, _ := f.st.GetOp(opID)
	if o.Status != store.OpNeedsAttention {
		t.Fatalf("op status = %q, want needs_attention", o.Status)
	}
}
