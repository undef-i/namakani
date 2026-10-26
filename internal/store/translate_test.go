package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'INBOX', 'inbox')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(10, 1, 1, 100, 'm10@x')`); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTranslationCacheRoundTripAndKeySeparation(t *testing.T) {
	st := openTestStore(t)
	row := &TranslationRow{Subject: "译标题", HTML: "<p>译文</p>", Text: "译文", SrcLang: "en"}
	if err := st.PutTranslation(10, "zh", "google", row); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := st.GetTranslation(10, "zh", "google")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Subject != row.Subject || got.HTML != row.HTML || got.Text != row.Text || got.SrcLang != "en" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if _, ok, _ := st.GetTranslation(10, "zh", "llm:openai:model-x"); ok {
		t.Fatal("a different engine key must miss the cache")
	}
	if _, ok, _ := st.GetTranslation(10, "en", "google"); ok {
		t.Fatal("a different target language must miss the cache")
	}
	if _, ok, _ := st.GetTranslation(11, "zh", "google"); ok {
		t.Fatal("a different message must miss the cache")
	}
}

func TestTranslationCacheOverwrite(t *testing.T) {
	st := openTestStore(t)
	if err := st.PutTranslation(10, "zh", "google", &TranslationRow{Subject: "一", SrcLang: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutTranslation(10, "zh", "google", &TranslationRow{Subject: "二", SrcLang: "en"}); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := st.GetTranslation(10, "zh", "google")
	if !ok || got.Subject != "二" {
		t.Fatalf("overwrite did not win: %+v ok=%v", got, ok)
	}
}

func TestAvatarCacheRoundTripIncludingNegative(t *testing.T) {
	st := openTestStore(t)
	if _, ok, _ := st.GetAvatar("noreply@example.com"); ok {
		t.Fatal("unknown sender must miss")
	}
	if err := st.PutAvatar("noreply@example.com", "gravatar", "image/png", []byte("PNGDATA"), 111); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetAvatar("noreply@example.com")
	if err != nil || !ok {
		t.Fatalf("get avatar: ok=%v err=%v", ok, err)
	}
	if got.Kind != "gravatar" || got.Mime != "image/png" || string(got.Data) != "PNGDATA" || got.CheckedAt != 111 {
		t.Fatalf("avatar round trip mismatch: %+v", got)
	}
	if err := st.PutAvatar("nobody@example.com", "", "", nil, 222); err != nil {
		t.Fatal(err)
	}
	neg, ok, _ := st.GetAvatar("nobody@example.com")
	if !ok || neg.Kind != "" || len(neg.Data) != 0 || neg.CheckedAt != 222 {
		t.Fatalf("negative cache row wrong: %+v ok=%v", neg, ok)
	}
}
