package gmail

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"namakani/internal/mail"
	"namakani/internal/store"

	"github.com/emersion/go-imap/v2"
)

func openDriverStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, kind, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'g', 'user@gmail.com', 'gmail', '', 0, 'none', '', 0, 'none', '', '', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Sent', 'sent'), (3, 1, 'Drafts', 'drafts'),
		(4, 1, 'Spam', 'junk'), (5, 1, 'Trash', 'trash'), (6, 1, 'Starred', 'flagged'),
		(7, 1, 'Important', ''), (8, 1, 'All Mail', 'all'), (9, 1, 'Work', ''), (10, 1, 'Work/Projects', '')`); err != nil {
		t.Fatal(err)
	}
	return st
}

func driverAccount() mail.Account {
	return mail.Account{ID: 1, Kind: "gmail", Email: "user@gmail.com", Password: testCreds().JSON()}
}

func connect(t *testing.T, st *store.Store) *Session {
	t.Helper()
	sess, err := Connect(st, driverAccount())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return sess
}

func seedBasic(t *testing.T, f *fakeGmail) {
	t.Helper()
	m1 := stdMsg("m1", "INBOX", "UNREAD", "STARRED")
	m1.hasAttach = true
	f.addMessage(m1)
	f.addMessage(stdMsg("m2", "INBOX"))
	f.addMessage(stdMsg("m3", "SENT"))
	f.addMessage(stdMsg("m4", "INBOX", "Label_1"))
	f.addMessage(stdMsg("m5", "TRASH"))
}

func envelopesBySubject(msgs []mail.Msg) map[string]mail.Msg {
	out := map[string]mail.Msg{}
	for _, m := range msgs {
		out[m.Subject] = m
	}
	return out
}

func TestListFoldersMapping(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	st := openDriverStore(t)
	sess := connect(t, st)
	folders, err := sess.ListFolders()
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}
	got := map[string]string{}
	for _, fi := range folders {
		got[fi.Name] = fi.SpecialUse
	}
	want := map[string]string{
		"INBOX": "inbox", "Sent": "sent", "Drafts": "drafts", "Spam": "junk",
		"Trash": "trash", "Starred": "flagged", "Important": "", "All Mail": "all",
		"Work": "", "Work/Projects": "",
	}
	if len(got) != len(want) {
		t.Fatalf("folders = %v; want exactly %v", got, want)
	}
	for name, special := range want {
		if got[name] != special {
			t.Fatalf("folder %q special = %q; want %q", name, got[name], special)
		}
	}
}

func TestFullSyncEnvelopesAndFlags(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)
	sess := connect(t, st)

	sd, err := sess.Select("INBOX")
	if err != nil {
		t.Fatalf("select inbox: %v", err)
	}
	if sd.NumMessages != 3 {
		t.Fatalf("inbox count = %d; want 3 (m1, m2, m4)", sd.NumMessages)
	}
	flags, err := sess.FetchFlags(0)
	if err != nil {
		t.Fatalf("fetch flags: %v", err)
	}
	uids := make([]uint32, 0, len(flags))
	seenByUID := map[uint32]uint32{}
	for _, fl := range flags {
		uids = append(uids, fl.UID)
		seenByUID[fl.UID] = fl.Flags
	}
	envs, err := sess.FetchEnvelopes(uids)
	if err != nil {
		t.Fatalf("fetch envelopes: %v", err)
	}
	bySubject := envelopesBySubject(envs)
	m1, ok := bySubject["Subject m1"]
	if !ok {
		t.Fatalf("envelopes = %v; m1 missing", envs)
	}
	if m1.MessageID != "m1@mail.test" {
		t.Fatalf("m1 message-id = %q; want m1@mail.test without brackets", m1.MessageID)
	}
	if m1.Flags&mail.FlagSeen != 0 {
		t.Fatalf("m1 is UNREAD; seen bit set in envelope flags %d", m1.Flags)
	}
	if m1.Flags&mail.FlagFlagged == 0 {
		t.Fatalf("m1 is STARRED; flagged bit missing in envelope flags %d", m1.Flags)
	}
	if !m1.HasAttach {
		t.Fatal("m1 has an attachment part; HasAttach = false")
	}
	if len(m1.From) != 1 || m1.From[0].Email != "alice@x.test" || m1.From[0].Name != "Alice" {
		t.Fatalf("m1 from = %+v; want Alice <alice@x.test>", m1.From)
	}
	if seenByUID[m1.UID]&mail.FlagSeen != 0 || seenByUID[m1.UID]&mail.FlagFlagged == 0 {
		t.Fatalf("m1 flags via FetchFlags = %d; want flagged and not seen", seenByUID[m1.UID])
	}
	m2 := bySubject["Subject m2"]
	if m2.Flags&mail.FlagSeen == 0 {
		t.Fatalf("m2 has no UNREAD label; seen bit missing in flags %d", m2.Flags)
	}
	m1UID := m1.UID

	sd, err = sess.Select("All Mail")
	if err != nil {
		t.Fatalf("select all mail: %v", err)
	}
	if sd.NumMessages != 4 {
		t.Fatalf("all mail count = %d; want 4 (trash excluded)", sd.NumMessages)
	}
	sd, err = sess.Select("Work")
	if err != nil {
		t.Fatalf("select work: %v", err)
	}
	if sd.NumMessages != 1 {
		t.Fatalf("work count = %d; want 1 (m4)", sd.NumMessages)
	}

	sess2 := connect(t, st)
	sd, err = sess2.Select("INBOX")
	if err != nil {
		t.Fatalf("select inbox session 2: %v", err)
	}
	flags2, err := sess2.FetchFlags(0)
	if err != nil {
		t.Fatal(err)
	}
	var uids2 []uint32
	for _, fl := range flags2 {
		uids2 = append(uids2, fl.UID)
	}
	envs2, err := sess2.FetchEnvelopes(uids2)
	if err != nil {
		t.Fatal(err)
	}
	if got := envelopesBySubject(envs2)["Subject m1"].UID; got != m1UID {
		t.Fatalf("m1 uid changed across sessions: %d -> %d; uids must be stable", m1UID, got)
	}
}

func syncOnce(t *testing.T, st *store.Store) *Session {
	t.Helper()
	sess := connect(t, st)
	names := []string{"INBOX", "Sent", "Drafts", "Spam", "Trash", "Starred", "Important", "All Mail", "Work", "Work/Projects"}
	for i, name := range names {
		folderID := int64(i + 1)
		if _, err := sess.Select(name); err != nil {
			t.Fatalf("select %s: %v", name, err)
		}
		flags, err := sess.FetchFlags(0)
		if err != nil {
			t.Fatalf("flags %s: %v", name, err)
		}
		var uids []uint32
		for _, fl := range flags {
			uids = append(uids, fl.UID)
		}
		envs, err := sess.FetchEnvelopes(uids)
		if err != nil {
			t.Fatalf("envelopes %s: %v", name, err)
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
				t.Fatalf("upsert %s/%s: %v", name, msg.Subject, err)
			}
		}
		if err := st.UpdateFolderSync(folderID, 1, 1000, 0, len(envs)); err != nil {
			t.Fatalf("folder sync state %s: %v", name, err)
		}
	}
	return sess
}

func TestHistoryIncrementalAppliesChanges(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)
	sess := syncOnce(t, st)
	mark := sess.HistoryMark()
	if mark == "" {
		t.Fatal("history mark after a full sync is empty; want the profile history id")
	}
	if err := sess.AckHistory(mark); err != nil {
		t.Fatalf("ack: %v", err)
	}
	stored, _ := st.GmailHistoryID(1)
	if stored != mark {
		t.Fatalf("stored history id = %q; want acked %q", stored, mark)
	}

	f.changeLabels("m2", []string{"STARRED"}, nil)
	f.addMessage(stdMsg("m6", "INBOX", "UNREAD"))
	f.deleteMsg("m1")
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	sess2 := connect(t, st)
	sd, err := sess2.Select("INBOX")
	if err != nil {
		t.Fatalf("select inbox after history: %v", err)
	}
	if sd.NumMessages != 3 {
		t.Fatalf("inbox count after history = %d; want 3 (m2, m4, m6)", sd.NumMessages)
	}
	if n := f.countCalls("messages?"); n != 0 {
		t.Fatalf("history sync used %d messages.list calls; want 0", n)
	}
	if n := f.countCalls("/history?"); n == 0 {
		t.Fatal("history sync never called history.list")
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
		t.Fatal(err)
	}
	bySubject := envelopesBySubject(envs)
	if _, ok := bySubject["Subject m1"]; ok {
		t.Fatal("m1 was deleted on the server but is still listed in INBOX")
	}
	m2, ok := bySubject["Subject m2"]
	if !ok {
		t.Fatal("m2 missing after history sync")
	}
	if m2.Flags&mail.FlagFlagged == 0 {
		t.Fatalf("m2 gained STARRED via history; flagged bit missing in flags %d", m2.Flags)
	}
	m6, ok := bySubject["Subject m6"]
	if !ok {
		t.Fatal("m6 arrived via history but has no envelope")
	}
	if m6.Flags&mail.FlagSeen != 0 {
		t.Fatalf("m6 is UNREAD; seen bit set in flags %d", m6.Flags)
	}
	mark2 := sess2.HistoryMark()
	if mark2 == "" || mark2 == mark {
		t.Fatalf("history mark after incremental = %q; want an advance from %q", mark2, mark)
	}
	if err := sess2.AckHistory(mark2); err != nil {
		t.Fatal(err)
	}
	stored, _ = st.GmailHistoryID(1)
	if stored != mark2 {
		t.Fatalf("stored history id = %q; want %q", stored, mark2)
	}
}

func TestHistoryExpiredFallsBackToFullList(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)
	sess := syncOnce(t, st)
	if err := sess.AckHistory(sess.HistoryMark()); err != nil {
		t.Fatal(err)
	}
	f.changeLabels("m2", []string{"STARRED"}, nil)
	f.expireHistory()
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()

	sess2 := connect(t, st)
	sd, err := sess2.Select("INBOX")
	if err != nil {
		t.Fatalf("select inbox after history expiry: %v", err)
	}
	if sd.NumMessages != 3 {
		t.Fatalf("inbox count = %d; want 3", sd.NumMessages)
	}
	if n := f.countCalls("messages?"); n == 0 {
		t.Fatal("expired history did not trigger a full list")
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
		t.Fatal(err)
	}
	if m2 := envelopesBySubject(envs)["Subject m2"]; m2.Flags&mail.FlagFlagged == 0 {
		t.Fatalf("after full relist m2 flags = %d; want flagged from the relisted state", m2.Flags)
	}
}

type modifyBody struct {
	Add    []string `json:"addLabelIds"`
	Remove []string `json:"removeLabelIds"`
}

func lastModify(t *testing.T, f *fakeGmail) modifyBody {
	t.Helper()
	calls := f.callsMatching("/modify")
	if len(calls) == 0 {
		t.Fatal("no modify call recorded")
	}
	var b modifyBody
	if err := json.Unmarshal([]byte(calls[len(calls)-1].Body), &b); err != nil {
		t.Fatalf("modify body: %v", err)
	}
	return b
}

func sameSet(a, b []string) bool {
	as := append([]string{}, a...)
	bs := append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func selectOne(t *testing.T, sess *Session, folder, subject string) mail.Msg {
	t.Helper()
	if _, err := sess.Select(folder); err != nil {
		t.Fatalf("select %s: %v", folder, err)
	}
	flags, err := sess.FetchFlags(0)
	if err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, fl := range flags {
		uids = append(uids, fl.UID)
	}
	envs, err := sess.FetchEnvelopes(uids)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := envelopesBySubject(envs)[subject]
	if !ok {
		t.Fatalf("%s not found in %s: %v", subject, folder, envs)
	}
	return m
}

func TestOperationTranslations(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)
	sess := connect(t, st)

	m2 := selectOne(t, sess, "INBOX", "Subject m2")
	if err := sess.StoreFlags([]uint32{m2.UID}, imap.FlagSeen, true); err != nil {
		t.Fatalf("store seen: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, nil) || !sameSet(b.Remove, []string{"UNREAD"}) {
		t.Fatalf("mark read modify = %+v; want remove UNREAD only", b)
	}
	if err := sess.StoreFlags([]uint32{m2.UID}, imap.FlagSeen, false); err != nil {
		t.Fatalf("store unseen: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, []string{"UNREAD"}) || !sameSet(b.Remove, nil) {
		t.Fatalf("mark unread modify = %+v; want add UNREAD only", b)
	}
	if err := sess.StoreFlag(m2.UID, imap.FlagFlagged, true); err != nil {
		t.Fatalf("store flagged: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, []string{"STARRED"}) || !sameSet(b.Remove, nil) {
		t.Fatalf("star modify = %+v; want add STARRED only", b)
	}

	m4 := selectOne(t, sess, "INBOX", "Subject m4")
	destUID, err := sess.MoveMessage(m4.UID, "Trash")
	if err != nil {
		t.Fatalf("move to trash: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, []string{"TRASH"}) || !sameSet(b.Remove, []string{"INBOX"}) {
		t.Fatalf("move to trash modify = %+v; want add TRASH remove INBOX", b)
	}
	wantUID, _ := st.AssignGmailUID(1, "Trash", "m4")
	if destUID != wantUID {
		t.Fatalf("move returned dest uid %d; want mapping uid %d", destUID, wantUID)
	}

	m2 = selectOne(t, sess, "INBOX", "Subject m2")
	if _, err := sess.MoveMessage(m2.UID, "All Mail"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, nil) || !sameSet(b.Remove, []string{"INBOX"}) {
		t.Fatalf("archive modify = %+v; want remove INBOX only", b)
	}

	m5 := selectOne(t, sess, "Trash", "Subject m5")
	if _, err := sess.MoveMessage(m5.UID, "Work"); err != nil {
		t.Fatalf("move trash to work: %v", err)
	}
	if b := lastModify(t, f); !sameSet(b.Add, []string{"Label_1"}) || !sameSet(b.Remove, []string{"TRASH"}) {
		t.Fatalf("trash to work modify = %+v; want add Label_1 remove TRASH", b)
	}

	m5 = selectOne(t, sess, "Work", "Subject m5")
	if err := sess.DeleteMessage(m5.UID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	dels := f.callsMatching("DELETE")
	found := false
	for _, c := range dels {
		if strings.Contains(c.Path, "/messages/m5") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no DELETE call for m5; calls = %v", f.calls)
	}
}

func TestSendRawDraftAppendAndFetchRaw(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	seedBasic(t, f)
	st := openDriverStore(t)
	sess := connect(t, st)

	raw := []byte("From: user@gmail.com\r\nTo: rcpt@x.test\r\nSubject: hello\r\n\r\nbody text\r\n")
	sentID, err := sess.SendRaw(raw)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sentID == "" {
		t.Fatal("send returned an empty message id")
	}
	sends := f.callsMatching("/messages/send")
	if len(sends) != 1 {
		t.Fatalf("send calls = %d; want 1", len(sends))
	}
	var sendBody struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal([]byte(sends[0].Body), &sendBody); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(sendBody.Raw)
	if err != nil || string(decoded) != string(raw) {
		t.Fatalf("sent raw decodes to %q, %v; want the original mime", decoded, err)
	}
	uid, err := sess.AssignUID("Sent", sentID)
	if err != nil || uid == 0 {
		t.Fatalf("assign sent uid = %d, %v", uid, err)
	}
	uid2, err := sess.AssignUID("Sent", sentID)
	if err != nil || uid2 != uid {
		t.Fatalf("assign sent uid not stable: %d -> %d", uid, uid2)
	}

	duid, err := sess.AppendMessage("Drafts", raw, imap.FlagSeen, imap.FlagDraft)
	if err != nil {
		t.Fatalf("append draft: %v", err)
	}
	if n := f.countCalls("/drafts?"); n != 1 {
		t.Fatalf("draft create calls = %d; want 1", n)
	}
	if duid == 0 {
		t.Fatal("append draft returned uid 0")
	}
	if _, err := sess.AppendMessage("INBOX", raw, imap.FlagSeen); err == nil {
		t.Fatal("append to INBOX succeeded; Gmail appends are drafts only")
	}

	m2 := selectOne(t, sess, "INBOX", "Subject m2")
	got, err := sess.FetchRaw(m2.UID)
	if err != nil {
		t.Fatalf("fetch raw: %v", err)
	}
	if !strings.Contains(string(got), "body of m2") {
		t.Fatalf("raw = %q; want m2's body", got)
	}
}
