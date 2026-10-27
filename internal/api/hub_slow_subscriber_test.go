package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHubDisconnectsSlowSubscriberInsteadOfDroppingEvents(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	const total = 40
	for i := 0; i < total; i++ {
		h.broadcast(map[string]any{"type": "ops", "n": i})
	}
	var seqs []int64
	closed := false
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				closed = true
				break loop
			}
			var ev map[string]any
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			seq, ok := ev["seq"].(float64)
			if !ok {
				t.Fatalf("event %s carries no sequence number", data)
			}
			seqs = append(seqs, int64(seq))
		case <-deadline:
			break loop
		}
	}
	if !closed {
		t.Fatal("slow subscriber connection was not closed: events beyond the buffer must not vanish silently, the subscription has to end so the client reconnects and rebuilds")
	}
	if len(seqs) == 0 || len(seqs) >= total {
		t.Fatalf("slow subscriber received %d of %d events before close, want a strict prefix", len(seqs), total)
	}
	for i, s := range seqs {
		if s != seqs[0]+int64(i) {
			t.Fatalf("delivered events are not a consecutive prefix: %v", seqs)
		}
	}
	h.mu.Lock()
	_, stillSubscribed := h.subs[ch]
	h.mu.Unlock()
	if stillSubscribed {
		t.Fatal("disconnected subscriber is still registered in the hub")
	}
}

func TestHubKeepingUpSubscriberReceivesEveryEvent(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	const total = 100
	var prev int64
	for i := 0; i < total; i++ {
		h.broadcast(map[string]any{"type": "ops", "n": i})
		select {
		case data := <-ch:
			var ev map[string]any
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			seq, ok := ev["seq"].(float64)
			if !ok {
				t.Fatalf("event %s carries no sequence number", data)
			}
			if prev != 0 && int64(seq) != prev+1 {
				t.Fatalf("keeping-up subscriber saw a gap: seq %v after %v", seq, prev)
			}
			prev = int64(seq)
		case <-time.After(2 * time.Second):
			t.Fatalf("keeping-up subscriber missed event %d", i)
		}
	}
	h.mu.Lock()
	_, stillSubscribed := h.subs[ch]
	h.mu.Unlock()
	if !stillSubscribed {
		t.Fatal("keeping-up subscriber was disconnected")
	}
}
