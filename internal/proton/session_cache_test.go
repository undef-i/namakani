package proton

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/emersion/hydroxide/protonmail"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type labelCall struct {
	label string
	ids   []string
}

type fakeProtonAPI struct {
	mu        sync.Mutex
	msgs      map[string]*protonmail.Message
	order     []string
	deleted   []string
	labeled   []labelCall
	unlabeled []labelCall
}

func newFakeProtonAPI(msgs ...*protonmail.Message) *fakeProtonAPI {
	f := &fakeProtonAPI{msgs: make(map[string]*protonmail.Message)}
	for _, m := range msgs {
		f.msgs[m.ID] = m
		f.order = append(f.order, m.ID)
	}
	return f
}

func (f *fakeProtonAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		label := r.URL.Query().Get("Label")
		_, desc := r.URL.Query()["Desc"]
		var out []*protonmail.Message
		for _, id := range f.order {
			m, ok := f.msgs[id]
			if !ok {
				continue
			}
			for _, l := range m.LabelIDs {
				if l == label {
					out = append(out, m)
					break
				}
			}
		}
		if !desc {
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"Code": 1000, "Total": len(out), "Messages": out,
		})
	})
	mux.HandleFunc("/messages/delete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct{ IDs []string }
		json.NewDecoder(r.Body).Decode(&req)
		f.deleted = append(f.deleted, req.IDs...)
		for _, id := range req.IDs {
			delete(f.msgs, id)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Code":1000}`))
	})
	record := func(dst *[]labelCall) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var req struct {
				LabelID string
				IDs     []string
			}
			json.NewDecoder(r.Body).Decode(&req)
			*dst = append(*dst, labelCall{label: req.LabelID, ids: req.IDs})
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"Code":1000}`))
		}
	}
	mux.HandleFunc("/messages/label", record(&f.labeled))
	mux.HandleFunc("/messages/unlabel", record(&f.unlabeled))
	return mux
}

func newCacheTestSession(t *testing.T, f *fakeProtonAPI) (*Session, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@proton.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@proton.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	c := &protonmail.Client{RootURL: srv.URL, HTTPClient: srv.Client()}
	return &Session{st: st, acct: mail.Account{ID: 1}, c: c}, st
}

func uidOf(t *testing.T, st *store.Store, folder, id string) uint32 {
	t.Helper()
	m, _, err := st.ProtonUIDMap(1, folder)
	if err != nil {
		t.Fatalf("ProtonUIDMap: %v", err)
	}
	uid, ok := m[id]
	if !ok {
		t.Fatalf("no uid for %s in %s (map %v)", id, folder, m)
	}
	return uid
}

func TestSelectThenDeleteTwiceInSameFolder(t *testing.T) {
	f := newFakeProtonAPI(
		&protonmail.Message{ID: "m1", Subject: "one", Unread: 1, LabelIDs: []string{protonmail.LabelTrash, protonmail.LabelAllMail}},
		&protonmail.Message{ID: "m2", Subject: "two", Unread: 1, LabelIDs: []string{protonmail.LabelTrash, protonmail.LabelAllMail}},
	)
	s, st := newCacheTestSession(t, f)
	sel, err := s.SelectRW("Trash")
	if err != nil {
		t.Fatalf("SelectRW: %v", err)
	}
	if sel.NumMessages != 2 {
		t.Fatalf("NumMessages=%d, want 2", sel.NumMessages)
	}
	uid1 := uidOf(t, st, "Trash", "m1")
	uid2 := uidOf(t, st, "Trash", "m2")
	if err := s.DeleteMessage(uid1); err != nil {
		t.Fatalf("first DeleteMessage: %v", err)
	}
	if err := s.DeleteMessage(uid2); err != nil {
		t.Fatalf("second DeleteMessage in the same selection: %v", err)
	}
	if len(f.deleted) != 2 || f.deleted[0] != "m1" || f.deleted[1] != "m2" {
		t.Fatalf("delete endpoint got %v, want [m1 m2]", f.deleted)
	}
	flags, err := s.FetchFlags(0)
	if err != nil {
		t.Fatalf("FetchFlags after deletes: %v", err)
	}
	if len(flags) != 0 {
		t.Fatalf("FetchFlags after deletes returned %d messages, want 0", len(flags))
	}
}

func TestMoveThenOperateInSourceFolder(t *testing.T) {
	f := newFakeProtonAPI(
		&protonmail.Message{ID: "m1", Subject: "one", Unread: 1, LabelIDs: []string{protonmail.LabelInbox, protonmail.LabelAllMail}},
		&protonmail.Message{ID: "m2", Subject: "two", Unread: 1, LabelIDs: []string{protonmail.LabelInbox, protonmail.LabelAllMail}},
	)
	s, st := newCacheTestSession(t, f)
	if _, err := s.SelectRW("INBOX"); err != nil {
		t.Fatalf("SelectRW: %v", err)
	}
	uid1 := uidOf(t, st, "INBOX", "m1")
	uid2 := uidOf(t, st, "INBOX", "m2")
	destUID, err := s.MoveMessage(uid1, "Archive")
	if err != nil {
		t.Fatalf("MoveMessage: %v", err)
	}
	if destUID == 0 {
		t.Fatalf("MoveMessage returned destUID 0")
	}
	if err := s.DeleteMessage(uid2); err != nil {
		t.Fatalf("DeleteMessage in source folder after move: %v", err)
	}
	found := func(calls []labelCall, label, id string) bool {
		for _, c := range calls {
			if c.label != label {
				continue
			}
			for _, x := range c.ids {
				if x == id {
					return true
				}
			}
		}
		return false
	}
	if !found(f.labeled, protonmail.LabelArchive, "m1") {
		t.Fatalf("label endpoint never labeled m1 as Archive: %+v", f.labeled)
	}
	if !found(f.unlabeled, protonmail.LabelInbox, "m1") {
		t.Fatalf("unlabel endpoint never removed m1 from Inbox: %+v", f.unlabeled)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "m2" {
		t.Fatalf("delete endpoint got %v, want [m2]", f.deleted)
	}
	envs, err := s.FetchEnvelopes([]uint32{uid1})
	if err != nil {
		t.Fatalf("FetchEnvelopes after move: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("moved message still visible in source folder: %+v", envs)
	}
}
