package gmail

import (
	"encoding/json"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"
)

func upsertEnvelopes(t *testing.T, st *store.Store, sess *Session, folder string, folderID int64) {
	t.Helper()
	if _, err := sess.Select(folder); err != nil {
		t.Fatalf("select %s: %v", folder, err)
	}
	flags, err := sess.FetchFlags(0)
	if err != nil {
		t.Fatalf("flags %s: %v", folder, err)
	}
	var uids []uint32
	for _, fl := range flags {
		uids = append(uids, fl.UID)
	}
	envs, err := sess.FetchEnvelopes(uids)
	if err != nil {
		t.Fatalf("envelopes %s: %v", folder, err)
	}
	for _, msg := range envs {
		fromJSON, _ := json.Marshal(msg.From)
		toJSON, _ := json.Marshal(msg.To)
		ccJSON, _ := json.Marshal(msg.Cc)
		row := &store.MessageRow{
			UID:       msg.UID,
			MessageID: msg.MessageID,
			ThreadKey: st.ComputeThreadKey(1, msg.MessageID, msg.InReplyTo, msg.Subject),
			Subject:   msg.Subject,
			FromJSON:  string(fromJSON),
			ToJSON:    string(toJSON),
			CcJSON:    string(ccJSON),
			DateTS:    msg.Date.Unix(),
			Flags:     msg.Flags,
			Size:      msg.Size,
			HasAttach: msg.HasAttach,
		}
		if _, _, err := st.UpsertMessage(1, folderID, row); err != nil {
			t.Fatalf("upsert %s/%s: %v", folder, msg.Subject, err)
		}
	}
}

func TestInitialSyncResumesFromStoredRows(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)

	sess1 := connect(t, st)
	upsertEnvelopes(t, st, sess1, "INBOX", 1)

	f.addMessage(stdMsg("m6", "INBOX"))
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	sess2 := connect(t, st)
	sd, err := sess2.Select("INBOX")
	if err != nil {
		t.Fatalf("select inbox session 2: %v", err)
	}
	if sd.NumMessages != 4 {
		t.Fatalf("inbox count = %d; want 4 (m1, m2, m4, m6)", sd.NumMessages)
	}
	for _, id := range []string{"m1", "m2", "m4"} {
		if n := f.countCalls("messages/" + id + "?"); n != 0 {
			t.Fatalf("stored message %s was refetched during folder build: %d gets", id, n)
		}
	}
	if n := f.countCalls("messages/m6?"); n != 1 {
		t.Fatalf("new message m6 fetched %d times during build; want 1", n)
	}

	flags, err := sess2.FetchFlags(0)
	if err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, fl := range flags {
		uids = append(uids, fl.UID)
	}
	envs, err := sess2.FetchEnvelopes(uids)
	if err != nil {
		t.Fatalf("envelopes session 2: %v", err)
	}
	for _, id := range []string{"m1", "m2", "m4"} {
		if n := f.countCalls("messages/" + id + "?"); n != 0 {
			t.Fatalf("stored message %s was refetched for its envelope: %d gets", id, n)
		}
	}
	bySubject := envelopesBySubject(envs)
	m1, ok := bySubject["Subject m1"]
	if !ok {
		t.Fatalf("m1 envelope missing after resume: %v", envs)
	}
	if m1.Flags&mail.FlagFlagged == 0 || m1.Flags&mail.FlagSeen != 0 {
		t.Fatalf("m1 flags after resume = %d; want flagged and unseen from the stored row", m1.Flags)
	}
	if !m1.HasAttach {
		t.Fatal("m1 HasAttach lost in the stored envelope")
	}
	if len(m1.From) != 1 || m1.From[0].Email != "alice@x.test" || m1.From[0].Name != "Alice" {
		t.Fatalf("m1 from after resume = %+v; want Alice <alice@x.test>", m1.From)
	}
	if _, ok := bySubject["Subject m6"]; !ok {
		t.Fatalf("m6 envelope missing after resume: %v", envs)
	}
}
