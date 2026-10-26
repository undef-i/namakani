package store

import (
	"path/filepath"
	"strings"
	"testing"

	"namakani/internal/mail"
)

// An oversize inbound body is stored truncated, with the truncation
// marker persisted on the message row (body_state = 2), while the
// message metadata row itself is untouched.
func TestSaveBodyTruncatedMarkerPersisted(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'INBOX', 'inbox')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id, subject) VALUES(7, 1, 1, 42, 'big@x', 'big one')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	raw := "From: A <a@x.test>\r\nSubject: big one\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.Repeat("body line\r\n", 500)
	parsed, err := mail.ParseBodyCapped([]byte(raw), 1000)
	if err != nil {
		t.Fatalf("ParseBodyCapped: %v", err)
	}
	if !parsed.Truncated {
		t.Fatal("parsed body not marked truncated")
	}
	if err := st.SaveBody(7, parsed.Text, parsed.HTML, mail.MakeSnippet(parsed.Text, parsed.HTML), parsed.Truncated); err != nil {
		t.Fatalf("SaveBody: %v", err)
	}

	var state int
	if err := st.DB.QueryRow("SELECT body_state FROM messages WHERE id = 7").Scan(&state); err != nil {
		t.Fatalf("read body_state: %v", err)
	}
	if state != 2 {
		t.Fatalf("body_state = %d, want 2 (truncated marker persisted)", state)
	}
	text, _, ok, err := st.GetBody(7)
	if err != nil || !ok {
		t.Fatalf("GetBody: ok=%v err=%v", ok, err)
	}
	if text != parsed.Text || len(text) > 1000 {
		t.Fatalf("stored text = %d bytes, want the capped %d bytes", len(text), len(parsed.Text))
	}
	var subject string
	if err := st.DB.QueryRow("SELECT subject FROM messages WHERE id = 7").Scan(&subject); err != nil || subject != "big one" {
		t.Fatalf("metadata subject = %q, %v; want it preserved", subject, err)
	}
}
