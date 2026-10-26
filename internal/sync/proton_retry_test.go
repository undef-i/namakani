package sync

import (
	"errors"
	"testing"
)

func TestProtonSyncerRetriesFailedFolderUntilItSucceeds(t *testing.T) {
	attempts := map[int64]int{}
	ps := &protonSyncer{syncOneFn: func(r folderRef) error {
		attempts[r.id]++
		if r.id == 7 && attempts[r.id] < 3 {
			return errors.New("folder fetch failed")
		}
		return nil
	}}
	a := folderRef{id: 7, name: "INBOX"}
	b := folderRef{id: 8, name: "Archive"}
	ps.sync(a)
	ps.sync(b)
	if len(ps.pending()) != 1 {
		t.Fatalf("pending = %v, want only the failed folder", ps.pending())
	}
	ps.retryOnce()
	if attempts[7] != 2 {
		t.Fatalf("attempts for the failed folder = %d, want a retry without waiting for a new event", attempts[7])
	}
	if len(ps.pending()) != 1 {
		t.Fatalf("pending after second failure = %v, want the folder still queued", ps.pending())
	}
	ps.retryOnce()
	if attempts[7] != 3 {
		t.Fatalf("attempts = %d, want a third try", attempts[7])
	}
	if len(ps.pending()) != 0 {
		t.Fatalf("pending after success = %v, want empty", ps.pending())
	}
	if attempts[8] != 1 {
		t.Fatalf("healthy folder attempts = %d, want exactly 1 (never queued)", attempts[8])
	}
}
