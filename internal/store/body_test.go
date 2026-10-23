package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestDeleteDraftRowVanishesBetweenRouteAndLocalDelete(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.DB.Close()
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'a', 'a@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'a@x.test', '', 0)`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES(1, 1, 'Drafts', 'drafts')`); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(7, 1, 1, 42, 'd@x')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := st.MessageRoute(7); err != nil {
		t.Fatalf("route before vanish: %v", err)
	}
	if _, err := st.DB.Exec(`DELETE FROM messages WHERE id = 7`); err != nil {
		t.Fatalf("vanish row: %v", err)
	}
	if _, err := st.MessageRoute(7); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("route after vanish = %v, want sql.ErrNoRows", err)
	}
	if err := st.DeleteLocalMessage(7); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("local delete after vanish = %v, want sql.ErrNoRows", err)
	}
	if _, err := st.MessageRoute(999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("route unknown id = %v, want sql.ErrNoRows", err)
	}
}
