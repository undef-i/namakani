package sync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emersion/hydroxide/protonmail"
)

type fakeEventSource struct {
	mu       sync.Mutex
	latest   string
	cursors  []string
	onSince  func(last string, call int) (*protonmail.Event, error)
	latestN  int
}

func (f *fakeEventSource) EventLatest() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestN++
	return f.latest, nil
}

func (f *fakeEventSource) EventSince(last string) (*protonmail.Event, error) {
	f.mu.Lock()
	f.cursors = append(f.cursors, last)
	call := len(f.cursors)
	fn := f.onSince
	f.mu.Unlock()
	return fn(last, call)
}

func (f *fakeEventSource) seenCursors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.cursors...)
}

func shrinkEventTimers(t *testing.T) {
	t.Helper()
	oldIdle, oldBase, oldMax, oldAge := protonEventIdleWait, protonEventErrBase, protonEventErrMax, protonEventRebaseAge
	protonEventIdleWait = 20 * time.Millisecond
	protonEventErrBase = 10 * time.Millisecond
	protonEventErrMax = 40 * time.Millisecond
	protonEventRebaseAge = 120 * time.Millisecond
	t.Cleanup(func() {
		protonEventIdleWait, protonEventErrBase, protonEventErrMax, protonEventRebaseAge = oldIdle, oldBase, oldMax, oldAge
	})
}

func TestProtonEventLoopKeepsCursorOnError(t *testing.T) {
	shrinkEventTimers(t)
	src := &fakeEventSource{latest: "e0"}
	src.onSince = func(last string, call int) (*protonmail.Event, error) {
		switch call {
		case 1:
			return nil, errors.New("network blip")
		case 2:
			return &protonmail.Event{ID: "e1", Messages: []*protonmail.EventMessage{{ID: "m1", Action: protonmail.EventCreate}}}, nil
		default:
			return &protonmail.Event{ID: "e1"}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *protonmail.Event, 4)
	go protonEventLoop(ctx, src, 4, out)
	select {
	case ev := <-out:
		if ev.ID != "e1" {
			t.Fatalf("delivered event id = %q, want e1", ev.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event after transient error was not delivered")
	}
	cursors := src.seenCursors()
	if len(cursors) < 2 || cursors[0] != "e0" || cursors[1] != "e0" {
		t.Fatalf("cursor advanced on error: %v", cursors)
	}
}

func TestProtonEventLoopRebasesAfterLongFailure(t *testing.T) {
	shrinkEventTimers(t)
	src := &fakeEventSource{latest: "e0"}
	src.onSince = func(last string, call int) (*protonmail.Event, error) {
		if last == "e9" {
			return &protonmail.Event{ID: "e9"}, nil
		}
		return nil, errors.New("still down")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *protonmail.Event, 4)
	go protonEventLoop(ctx, src, 4, out)
	time.AfterFunc(100*time.Millisecond, func() {
		src.mu.Lock()
		src.latest = "e9"
		src.mu.Unlock()
	})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-out:
			if ev.Refresh&protonmail.EventRefreshMail == 0 {
				t.Fatalf("expected refresh event after rebase, got %+v", ev)
			}
			for i := 0; i < 100; i++ {
				found := false
				for _, c := range src.seenCursors() {
					if c == "e9" {
						found = true
					}
				}
				if found {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("cursor never moved to rebased latest: %v", src.seenCursors())
		case <-deadline:
			t.Fatal("no rebase after long failure")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestProtonEventLoopDiscoveryLatencyBoundedByIdleWait(t *testing.T) {
	shrinkEventTimers(t)
	protonEventIdleWait = 300 * time.Millisecond
	src := &fakeEventSource{latest: "e0"}
	var availAt time.Time
	var once sync.Once
	src.onSince = func(last string, call int) (*protonmail.Event, error) {
		if call == 1 {
			once.Do(func() { availAt = time.Now() })
			return &protonmail.Event{ID: "e0"}, nil
		}
		return &protonmail.Event{ID: "e1", Messages: []*protonmail.EventMessage{{ID: "m1", Action: protonmail.EventCreate}}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *protonmail.Event, 4)
	go protonEventLoop(ctx, src, 4, out)
	select {
	case <-out:
		delay := time.Since(availAt)
		if delay > 500*time.Millisecond {
			t.Fatalf("discovery delay %v exceeds idle wait bound", delay)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not delivered")
	}
}
