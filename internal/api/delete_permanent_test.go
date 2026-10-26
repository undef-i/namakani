package api

import (
	"net/http"
	"testing"

	"namakani/internal/store"
)

func TestDeletePermanentFromInboxExpunges(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	rec := f.postDelete(t, 10, `{"permanent":true,"client_key":"perm-inbox"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("permanent delete from INBOX status = %d, body %s", rec.Code, rec.Body.String())
	}
	opID := opIDOf(t, rec)
	o, _ := f.st.GetOp(opID)
	if o.Type != store.OpExpunge {
		t.Fatalf("permanent delete from INBOX created op type=%q, want expunge", o.Type)
	}
	f.srv.drainAccount(nil, 1)
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("local row survived a drained permanent delete from INBOX")
	}
	calls := f.p.callsOf("DeleteMessage")
	if len(calls) != 1 || calls[0].Folder != "INBOX" || len(calls[0].UIDs) != 1 || calls[0].UIDs[0] != 100 {
		t.Fatalf("provider DeleteMessage calls = %+v, want exactly one in INBOX for uid 100", calls)
	}
}

func TestDeletePermanentFromArchiveExpunges(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 2, "Archive", 100, 0)
	rec := f.postDelete(t, 10, `{"permanent":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("permanent delete from Archive status = %d, body %s", rec.Code, rec.Body.String())
	}
	o, _ := f.st.GetOp(opIDOf(t, rec))
	if o.Type != store.OpExpunge {
		t.Fatalf("permanent delete from Archive created op type=%q, want expunge", o.Type)
	}
	f.srv.drainAccount(nil, 1)
	if _, err := f.st.MessageRoute(10); err == nil {
		t.Fatal("local row survived a drained permanent delete from Archive")
	}
}

func TestDeletePermanentSameKeyCoalesces(t *testing.T) {
	f := newDrainFixture(t)
	f.addMessage(t, 10, 1, "INBOX", 100, 0)
	first := opIDOf(t, f.postDelete(t, 10, `{"permanent":true,"client_key":"perm-dup"}`))
	second := opIDOf(t, f.postDelete(t, 10, `{"permanent":true,"client_key":"perm-dup"}`))
	if second != first {
		t.Fatalf("a retried permanent delete with the same client key created op %d after op %d; want the same op", second, first)
	}
	var n int
	_ = f.st.DB.QueryRow("SELECT COUNT(*) FROM ops WHERE message_id = 10 AND status IN ('pending','in_flight','needs_attention')").Scan(&n)
	if n != 1 {
		t.Fatalf("active ops for the message = %d, want exactly 1", n)
	}
}
