package sync

import (
	"testing"

	"github.com/emersion/hydroxide/protonmail"
)

func affectedPairs() map[string]string {
	return map[string]string{"INBOX": "0", "All Mail": "1", "Archive": "2"}
}

func noCounts() (map[string]int, map[string]int) {
	return map[string]int{"0": 3, "1": 3, "2": 0}, map[string]int{"INBOX": 3, "All Mail": 3, "Archive": 0}
}

func TestProtonAffectedFallsBackToKnownFoldersForPureReadChange(t *testing.T) {
	unread := 0
	ev := &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:     "pm-1",
		Action: protonmail.EventUpdate,
		Updated: &protonmail.EventMessageUpdate{
			Unread: &unread,
		},
	}}}
	counts, totals := noCounts()
	got := computeProtonAffected(affectedPairs(), counts, totals, ev, func(id string) ([]string, error) {
		if id != "pm-1" {
			t.Fatalf("folders lookup for %q, want pm-1", id)
		}
		return []string{"INBOX", "All Mail"}, nil
	})
	if !got["INBOX"] || !got["All Mail"] {
		t.Fatalf("affected = %v, want INBOX and All Mail: a pure read/unread update carries no label data, so the message's known folders are the only correct target", got)
	}
}

func TestProtonAffectedRecoversSingleSidedLabelDiff(t *testing.T) {
	ev := &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:     "pm-2",
		Action: protonmail.EventUpdate,
		Updated: &protonmail.EventMessageUpdate{
			LabelIDsAdded: []string{"2"},
		},
	}}}
	counts, totals := noCounts()
	got := computeProtonAffected(affectedPairs(), counts, totals, ev, func(id string) ([]string, error) {
		return nil, nil
	})
	if !got["Archive"] {
		t.Fatalf("affected = %v, want Archive: a single-sided label diff must not be dropped", got)
	}
}

func TestProtonAffectedCreateUsesLabelIDs(t *testing.T) {
	ev := &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:      "pm-3",
		Action:  protonmail.EventCreate,
		Created: &protonmail.Message{LabelIDs: []string{"0"}},
	}}}
	counts, totals := noCounts()
	got := computeProtonAffected(affectedPairs(), counts, totals, ev, func(id string) ([]string, error) {
		return nil, nil
	})
	if !got["INBOX"] || len(got) != 1 {
		t.Fatalf("affected = %v, want exactly INBOX", got)
	}
}

func TestProtonAffectedDeleteMarksKnownFolders(t *testing.T) {
	ev := &protonmail.Event{Messages: []*protonmail.EventMessage{{
		ID:     "pm-4",
		Action: protonmail.EventDelete,
	}}}
	counts, totals := noCounts()
	got := computeProtonAffected(affectedPairs(), counts, totals, ev, func(id string) ([]string, error) {
		if id != "pm-4" {
			t.Fatalf("folders lookup for %q, want pm-4", id)
		}
		return []string{"INBOX"}, nil
	})
	if !got["INBOX"] {
		t.Fatalf("affected = %v, want INBOX: a delete event whose folder counts are unchanged (one deleted, one created) must still mark the deleted message's known folders, or the row lingers until the periodic sweep", got)
	}
}
