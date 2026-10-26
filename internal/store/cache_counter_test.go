package store

import (
	"path/filepath"
	"testing"
)

func TestCacheUsageCounterMatchesFullSum(t *testing.T) {
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
	for _, m := range []struct{ id, uid int }{{1, 41}, {2, 42}, {3, 43}, {4, 44}} {
		if _, err := st.DB.Exec(`INSERT INTO messages(id, account_id, folder_id, uid, message_id) VALUES(?, 1, 1, ?, 'x')`, m.id, m.uid); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	fullSum := func() int64 {
		var n int64
		if err := st.DB.QueryRow(`SELECT COALESCE(SUM(LENGTH(text_body) + LENGTH(html_body)), 0) FROM bodies`).Scan(&n); err != nil {
			t.Fatalf("sum: %v", err)
		}
		return n
	}
	check := func(step string) {
		got, _, err := st.CacheUsage()
		if err != nil {
			t.Fatalf("%s: usage: %v", step, err)
		}
		if want := fullSum(); got != want {
			t.Fatalf("%s: usage = %d, want %d", step, got, want)
		}
		got2, _, err := st.CacheUsage()
		if err != nil {
			t.Fatalf("%s: usage2: %v", step, err)
		}
		if got2 != got {
			t.Fatalf("%s: second usage = %d, want %d", step, got2, got)
		}
	}
	check("empty")
	if err := st.SaveBody(1, "hello", "<b>hello</b>", "", false); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	check("save1")
	if err := st.SaveBody(2, "中文正文测试", "", "", false); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	check("save2")
	if err := st.SaveBody(1, "hi", "", "", false); err != nil {
		t.Fatalf("overwrite 1: %v", err)
	}
	check("overwrite1")
	if err := st.SaveBody(3, "third body", "<p>third</p>", "", false); err != nil {
		t.Fatalf("save 3: %v", err)
	}
	check("save3")
	if err := st.EvictBody(2); err != nil {
		t.Fatalf("evict 2: %v", err)
	}
	check("evict2")
	if err := st.DeleteMessage(1, 43); err != nil {
		t.Fatalf("delete msg 3: %v", err)
	}
	check("delete3")
	if err := st.DeleteLocalMessage(1); err != nil {
		t.Fatalf("local delete 1: %v", err)
	}
	check("localdelete1")
	if err := st.SaveBody(4, "fourth", "", "", false); err != nil {
		t.Fatalf("save 4: %v", err)
	}
	check("save4")
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	check("wipe")
}
