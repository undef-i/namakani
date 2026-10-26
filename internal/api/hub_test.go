package api

import (
	"encoding/json"
	"testing"
)

func TestHubBroadcastStampsIncreasingSequence(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	h.broadcast(map[string]any{"type": "flags"})
	h.broadcast(map[string]any{"type": "removed"})
	var seqs []int64
	for range 2 {
		data := <-ch
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		seq, ok := ev["seq"].(float64)
		if !ok {
			t.Fatalf("event %s carries no sequence number: a dropped event would be undetectable", data)
		}
		seqs = append(seqs, int64(seq))
	}
	if seqs[1] != seqs[0]+1 {
		t.Fatalf("sequence = %v, want consecutive values so a gap reveals a dropped event", seqs)
	}
}
