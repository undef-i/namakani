package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"namakani/internal/mail"

	"github.com/emersion/go-imap/v2"
)

func TestSendRetryAfterCrashWindowResendsIdenticalBytes(t *testing.T) {
	f := newDrainFixture(t)
	seams := f.armSend(t)
	var raws [][]byte
	var mids []string
	f.srv.sendDeliver = func(k []byte, out *outgoing) (string, error) {
		seams.deliverCalls++
		raws = append(raws, append([]byte(nil), out.raw...))
		mids = append(mids, out.messageID)
		return "provider-sent-1", nil
	}
	f.srv.sendFileCopy = func(k []byte, out *outgoing, sentID string) (int64, error) {
		return 0, errors.New("store unavailable")
	}
	opID, err := f.srv.enqueueSendOp(f.srv.keys["tok"], composeReq{
		AccountID: 1, To: "b@x.test", Subject: "hi", Text: "body",
	}, 0, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	op, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if f.srv.executeSend(f.srv.keys["tok"], op) {
		t.Fatal("first attempt must fail at the sent-copy step")
	}
	op, err = f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	var sp sendOpPayload
	if err := json.Unmarshal([]byte(op.Payload), &sp); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	sp.Sent = false
	sp.SentID = ""
	rawPayload, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET payload = ? WHERE id = ?", string(rawPayload), opID); err != nil {
		t.Fatalf("rewind sent bookkeeping: %v", err)
	}
	f.srv.sendFileCopy = func(k []byte, out *outgoing, sentID string) (int64, error) {
		return 777, nil
	}
	op, err = f.st.GetOp(opID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	if !f.srv.executeSend(f.srv.keys["tok"], op) {
		t.Fatal("second attempt must succeed")
	}
	if seams.deliverCalls != 2 {
		t.Fatalf("deliver calls = %d, want 2 (the crash window forces one redelivery; the defense is a stable identity, not hope)", seams.deliverCalls)
	}
	if mids[0] != mids[1] {
		t.Fatalf("message id changed across attempts: %q then %q; a redelivery must carry the same Message-ID so receivers can dedupe", mids[0], mids[1])
	}
	if !bytes.Equal(raws[0], raws[1]) {
		t.Fatal("raw bytes differ across attempts; a redelivery must resend the finalized message byte for byte")
	}
}

func TestAppendCopyDeduplicatesByMessageID(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	if _, err := f.st.DB.Exec("INSERT INTO folders(id, account_id, name, special_use) VALUES(5, 1, 'Sent', 'sent')"); err != nil {
		t.Fatalf("insert sent folder: %v", err)
	}
	out := &outgoing{
		raw:       []byte("From: a@x.test\r\nTo: b@x.test\r\nSubject: hi\r\n\r\nbody"),
		messageID: "fixed-mid@x.test",
		subject:   "hi",
		text:      "body",
		from:      mail.Addr{Email: "a@x.test"},
		to:        []mail.Addr{{Email: "b@x.test"}},
	}
	id1, err := f.srv.appendCopy(nil, 1, "sent", []imap.Flag{imap.FlagSeen}, mail.FlagSeen, out)
	if err != nil {
		t.Fatalf("first appendCopy: %v", err)
	}
	id2, err := f.srv.appendCopy(nil, 1, "sent", []imap.Flag{imap.FlagSeen}, mail.FlagSeen, out)
	if err != nil {
		t.Fatalf("second appendCopy: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("appendCopy returned %d then %d for the same Message-ID; the retry must reuse the filed copy", id1, id2)
	}
	if n := len(f.p.callsOf("AppendMessage")); n != 1 {
		t.Fatalf("provider AppendMessage calls = %d, want 1 (no duplicate remote copy)", n)
	}
	var total int
	if err := f.st.DB.QueryRow("SELECT total FROM folders WHERE id = 5").Scan(&total); err != nil {
		t.Fatalf("folder total: %v", err)
	}
	if total != 1 {
		t.Fatalf("sent folder total = %d, want 1", total)
	}
}

type stubAssigner struct{ uid uint32 }

func (a stubAssigner) AssignUID(folder, id string) (uint32, error) { return a.uid, nil }

func TestFileSentCopyCountsOnceOnRetry(t *testing.T) {
	f := newDrainFixture(t)
	f.armSend(t)
	if _, err := f.st.DB.Exec("INSERT INTO folders(id, account_id, name, special_use) VALUES(5, 1, 'Sent', 'sent')"); err != nil {
		t.Fatalf("insert sent folder: %v", err)
	}
	out := &outgoing{
		raw:       []byte("From: a@x.test\r\n\r\nbody"),
		messageID: "fixed-mid@x.test",
		subject:   "hi",
		text:      "body",
		from:      mail.Addr{Email: "a@x.test"},
		to:        []mail.Addr{{Email: "b@x.test"}},
	}
	acct := mail.Account{ID: 1, Email: "a@x.test"}
	id1, err := f.srv.fileSentCopy(acct, stubAssigner{uid: 42}, "provider-sent-1", out)
	if err != nil {
		t.Fatalf("first fileSentCopy: %v", err)
	}
	id2, err := f.srv.fileSentCopy(acct, stubAssigner{uid: 42}, "provider-sent-1", out)
	if err != nil {
		t.Fatalf("second fileSentCopy: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("fileSentCopy returned %d then %d for the same sent id", id1, id2)
	}
	var total int
	if err := f.st.DB.QueryRow("SELECT total FROM folders WHERE id = 5").Scan(&total); err != nil {
		t.Fatalf("folder total: %v", err)
	}
	if total != 1 {
		t.Fatalf("sent folder total = %d after a retried filing, want 1", total)
	}
}
