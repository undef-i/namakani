package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWipeFolderRemovesAttachmentFiles(t *testing.T) {
	st := opsTestStore(t)
	dir := filepath.Join(st.AttachmentsDir(), "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(dir, "att.bin")
	if err := os.WriteFile(file, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}
	if _, err := st.DB.Exec(`INSERT INTO attachments(message_id, part_path, filename, mime, size, local_path)
		VALUES(10, '1', 'att.bin', 'application/octet-stream', 7, ?)`, file); err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	if err := st.WipeFolder(1); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("attachment file still on disk after wipe (stat err = %v): a folder wipe must not leave orphaned files behind", err)
	}
	var n int
	if err := st.DB.QueryRow("SELECT COUNT(*) FROM messages WHERE folder_id = 1").Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 0 {
		t.Fatalf("messages after wipe = %d, want 0", n)
	}
}
