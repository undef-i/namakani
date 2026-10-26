package store

import (
	"strings"
	"testing"
)

func TestCacheStatsJoinUsesAttachmentIndex(t *testing.T) {
	st := opsTestStore(t)
	if _, err := st.DB.Exec(`INSERT INTO attachments(message_id, part_path, filename, mime, size)
		VALUES(10, '1', 'a.bin', 'application/octet-stream', 3)`); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	stats, err := st.AccountCacheStats(1)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.AttachmentsTotal != 1 {
		t.Fatalf("attachments total = %d, want 1", stats.AttachmentsTotal)
	}
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM attachments a
		JOIN messages m ON m.id = a.message_id JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = 1 AND f.special_use <> 'junk'`)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if strings.Contains(plan.String(), "SCAN a") {
		t.Fatalf("attachments join still scans the whole attachments table:\n%s", plan.String())
	}
}

func TestParentLookupUsesAccountMessageIndex(t *testing.T) {
	st := opsTestStore(t)
	rows, err := st.DB.Query(`EXPLAIN QUERY PLAN SELECT thread_key, subject FROM messages WHERE account_id = 1 AND message_id = ? AND thread_key <> '' LIMIT 1`, "<x@y>")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if !strings.Contains(plan.String(), "idx_messages_acct_msgid") {
		t.Fatalf("parent lookup does not use the (account_id, message_id) index:\n%s", plan.String())
	}
}
