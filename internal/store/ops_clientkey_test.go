package store

import (
	"errors"
	"testing"
)

func TestClientKeyReuseMustMatchTheOriginalOperation(t *testing.T) {
	st := opsTestStore(t)
	seen := true
	id, _, err := st.EnqueueFlags(1, 10, &seen, nil, "key-1")
	if err != nil {
		t.Fatalf("enqueue flags: %v", err)
	}
	again, _, err := st.EnqueueFlags(1, 10, &seen, nil, "key-1")
	if err != nil || again != id {
		t.Fatalf("exact replay = (%d, %v), want (%d, nil)", again, err, id)
	}
	if _, _, err := st.EnqueueFlags(1, 11, &seen, nil, "key-1"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("same key on another message: err = %v, want ErrClientKeyConflict (a replayed key must never silently swallow a different request)", err)
	}
	unseen := false
	if _, _, err := st.EnqueueFlags(1, 10, &unseen, nil, "key-1"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("same key with a different flag change: err = %v, want ErrClientKeyConflict", err)
	}
	if _, err := st.EnqueueExpunge(1, 10, "key-1"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("same key on another op type: err = %v, want ErrClientKeyConflict", err)
	}
}

func TestClientKeyMoveAndSendSemantics(t *testing.T) {
	st := opsTestStore(t)
	moveID, err := st.EnqueueMove(1, 10, 2, "key-2")
	if err != nil {
		t.Fatalf("enqueue move: %v", err)
	}
	again, err := st.EnqueueMove(1, 10, 2, "key-2")
	if err != nil || again != 0 {
		t.Fatalf("move replay = (%d, %v), want (0, nil): the message is already at the destination, so a replay is a no-op, and op %d stays the only move", again, err, moveID)
	}
	if _, err := st.EnqueueMove(1, 10, 3, "key-2"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("move key with a different destination: err = %v, want ErrClientKeyConflict", err)
	}
	if _, err := st.EnqueueMove(1, 11, 3, "key-2"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("move key on another message/destination: err = %v, want ErrClientKeyConflict", err)
	}
	sendID, err := st.EnqueueSend(1, `{"v":1}`, 0, "key-3")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	againSend, err := st.EnqueueSend(1, `{"v":1}`, 0, "key-3")
	if err != nil || againSend != sendID {
		t.Fatalf("send replay = (%d, %v), want (%d, nil)", againSend, err, sendID)
	}
	if _, err := st.EnqueueSend(1, `{"v":2}`, 0, "key-3"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("send key with a different payload: err = %v, want ErrClientKeyConflict", err)
	}
	if _, err := st.EnqueueSend(2, `{"v":1}`, 0, "key-3"); !errors.Is(err, ErrClientKeyConflict) {
		t.Fatalf("send key on another account: err = %v, want ErrClientKeyConflict", err)
	}
}
