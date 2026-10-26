package store

import (
	"path/filepath"
	"testing"
)

func seedThreadAccounts(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if _, err := st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at) VALUES
		(1, 'p', 'p@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'p@x.test', '', 0),
		(2, 'i', 'i@x.test', 'h', 993, 'tls', 'h', 465, 'tls', 'i@x.test', '', 0)`); err != nil {
		t.Fatalf("insert accounts: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(11, 1, 'INBOX', 'inbox'), (12, 1, 'Trash', 'trash'), (13, 1, 'All Mail', 'all'),
		(21, 2, 'INBOX', 'inbox'), (22, 2, 'Trash', 'trash')`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	seed := `INSERT INTO messages(id, account_id, folder_id, uid, message_id, thread_key, date_ts, flags) VALUES `
	rows := []string{
		`(101, 1, 11, 1, 'a1@x', 'root@x', 1000, 1)`,
		`(102, 1, 11, 2, 'a2@x', 'root@x', 2000, 0)`,
		`(103, 1, 12, 3, 'a3@x', 'root@x', 3000, 0)`,
		`(104, 1, 12, 4, 'a4@x', 'root@x', 4000, 1)`,
		`(105, 1, 12, 5, 'a5@x', 'root@x', 5000, 1)`,
		`(106, 1, 12, 6, 'a6@x', 'root@x', 6000, 1)`,
		`(107, 1, 12, 7, 'a7@x', 'root@x', 7000, 1)`,
		`(108, 1, 12, 8, 'a8@x', 'root@x', 8000, 1)`,
		`(109, 1, 12, 9, 'a9@x', 'root@x', 9000, 1)`,
		`(201, 1, 13, 201, 'a1@x', 'root@x', 1000, 1)`,
		`(202, 1, 13, 202, 'a2@x', 'root@x', 2000, 0)`,
		`(203, 1, 13, 203, 'a3@x', 'root@x', 3000, 0)`,
		`(204, 1, 13, 204, 'a4@x', 'root@x', 4000, 1)`,
		`(205, 1, 13, 205, 'a5@x', 'root@x', 5000, 1)`,
		`(206, 1, 13, 206, 'a6@x', 'root@x', 6000, 1)`,
		`(207, 1, 13, 207, 'a7@x', 'root@x', 7000, 1)`,
		`(208, 1, 13, 208, 'a8@x', 'root@x', 8000, 1)`,
		`(209, 1, 13, 209, 'a9@x', 'root@x', 9000, 1)`,
		`(210, 1, 13, 210, 'solo@x', 'solo@x', 500, 0)`,
		`(220, 1, 11, 20, '', '', 1500, 0)`,
		`(301, 2, 21, 1, 'b1@x', 'root@x', 1000, 1)`,
		`(302, 2, 21, 2, 'b2@x', 'root@x', 2000, 0)`,
		`(303, 2, 22, 3, 'b3@x', 'root@x', 3000, 0)`,
		`(304, 2, 22, 4, 'b4@x', 'root@x', 4000, 1)`,
		`(305, 2, 22, 5, 'b5@x', 'root@x', 5000, 1)`,
		`(306, 2, 22, 6, 'b6@x', 'root@x', 6000, 1)`,
		`(307, 2, 22, 7, 'b7@x', 'root@x', 7000, 1)`,
		`(308, 2, 22, 8, 'b8@x', 'root@x', 8000, 1)`,
		`(309, 2, 22, 9, 'b9@x', 'root@x', 9000, 1)`,
	}
	for _, r := range rows {
		if _, err := st.DB.Exec(seed + r); err != nil {
			t.Fatalf("seed %s: %v", r, err)
		}
	}
	return st
}

func TestThreadMembersPicksCanonicalRowPerMessage(t *testing.T) {
	st := seedThreadAccounts(t)
	got, err := st.ThreadMembers(1, "root@x")
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	want := []int64{101, 102, 103, 104, 105, 106, 107, 108, 109}
	if len(got) != len(want) {
		t.Fatalf("members = %v, want exactly %v: canonical rows only, All Mail mirrors merged, trash members included", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("members = %v, want %v in date order", got, want)
		}
	}
	got, err = st.ThreadMembers(2, "root@x")
	if err != nil {
		t.Fatalf("members account 2: %v", err)
	}
	want = []int64{301, 302, 303, 304, 305, 306, 307, 308, 309}
	if len(got) != len(want) {
		t.Fatalf("account 2 members = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("account 2 members = %v, want %v", got, want)
		}
	}
}

func TestThreadMembersAllOnlyAndUnkeyedSingletons(t *testing.T) {
	st := seedThreadAccounts(t)
	got, err := st.ThreadMembers(1, "solo@x")
	if err != nil {
		t.Fatalf("members solo: %v", err)
	}
	if len(got) != 1 || got[0] != 210 {
		t.Fatalf("members solo = %v, want [210]: a message with only an All Mail row is represented by that row", got)
	}
	got, err = st.ThreadMembers(1, "id:220")
	if err != nil {
		t.Fatalf("members id:220: %v", err)
	}
	if len(got) != 1 || got[0] != 220 {
		t.Fatalf("members id:220 = %v, want [220]", got)
	}
}

func TestThreadUniqueCountMergesMirrors(t *testing.T) {
	st := seedThreadAccounts(t)
	n, err := st.ThreadUniqueCount(1, "root@x")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 9 {
		t.Fatalf("unique count = %d, want 9, not 18 rows", n)
	}
	n, err = st.ThreadUniqueCount(1, "solo@x")
	if err != nil {
		t.Fatalf("count solo: %v", err)
	}
	if n != 1 {
		t.Fatalf("unique count solo = %d, want 1", n)
	}
}

func TestThreadGroupCountsWholeGroup(t *testing.T) {
	st := seedThreadAccounts(t)
	got, err := st.ThreadGroupCounts(1, []string{"root@x", "solo@x"})
	if err != nil {
		t.Fatalf("group counts: %v", err)
	}
	if gc := got["root@x"]; gc.Count != 9 || gc.Unread != 2 {
		t.Fatalf("group root@x = %+v, want {9 2}: whole group account-wide, mirrors merged, trash members counted", gc)
	}
	if gc := got["solo@x"]; gc.Count != 1 || gc.Unread != 1 {
		t.Fatalf("group solo@x = %+v, want {1 1}", gc)
	}
	got, err = st.ThreadGroupCounts(2, []string{"root@x"})
	if err != nil {
		t.Fatalf("group counts account 2: %v", err)
	}
	if gc := got["root@x"]; gc.Count != 9 || gc.Unread != 2 {
		t.Fatalf("account 2 group root@x = %+v, want {9 2}", gc)
	}
}
