package tuta

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type eventState struct {
	mu        sync.Mutex
	committed map[string]string
	pending   map[string]string
}

func (s *eventState) noteBatch(group, batch string) {
	s.mu.Lock()
	s.pending[group] = batch
	s.mu.Unlock()
}

func (s *eventState) mark() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.pending))
	for g, b := range s.pending {
		out[g] = b
	}
	return out
}

func (s *eventState) ack(mark map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for g, b := range mark {
		if s.pending[g] == b {
			s.committed[g] = b
		}
	}
}

func (s *eventState) connectParams() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.committed) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.committed))
	for group, batch := range s.committed {
		parts = append(parts, group+"="+batch)
	}
	return strings.Join(parts, ";")
}

func (s *Session) EventMark() map[string]string {
	if s.evState == nil {
		return nil
	}
	return s.evState.mark()
}

func (s *Session) AckEvents(mark map[string]string) {
	if s.evState != nil && mark != nil {
		s.evState.ack(mark)
	}
}

func (s *Session) eventURL(state *eventState) string {
	q := url.Values{}
	q.Set("modelVersions", modelVersionSys+"."+modelVersionTutanota)
	q.Set("clientVersion", clientVersion)
	q.Set("userId", s.userID)
	q.Set("accessToken", s.accessToken)
	if cp := state.connectParams(); cp != "" {
		q.Set("groupsToLastEventBatchIds", cp)
	}
	return "wss://app.tuta.com/event?" + q.Encode()
}

func randomInRange(minS, maxS int) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxS-minS+1)))
	if err != nil {
		return time.Duration(minS) * time.Second
	}
	return time.Duration(minS+int(n.Int64())) * time.Second
}

func reconnectDelay(failures int) time.Duration {
	switch {
	case failures <= 1:
		return randomInRange(5, 10)
	case failures == 2:
		return randomInRange(20, 40)
	default:
		return randomInRange(60, 120)
	}
}

const healthyUptime = 120 * time.Second

type reconnectStreak struct {
	failures int
}

func (s *reconnectStreak) record(uptime time.Duration) time.Duration {
	if uptime >= healthyUptime {
		s.failures = 0
	}
	s.failures++
	return reconnectDelay(s.failures)
}

func terminalCloseCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	code := websocket.CloseStatus(err)
	return int(code), code == 4401 || code == 4470 || code == 4472
}

func (s *Session) WatchEvents(stop <-chan struct{}, notify func(), onTerminal func(error)) {
	if s.evState != nil {
		s.InvalidateCaches()
	}
	state := &eventState{committed: make(map[string]string), pending: make(map[string]string)}
	s.evState = state
	var streak reconnectStreak
	for {
		select {
		case <-stop:
			return
		default:
		}
		started := time.Now()
		err := s.eventLoop(stop, state, notify)
		if code, terminal := terminalCloseCode(err); terminal {
			if onTerminal != nil {
				onTerminal(&RestError{StatusCode: 401, Name: "WebSocketAuthClosed", Message: fmt.Sprintf("event stream closed with code %d", code)})
			}
			return
		}
		s.InvalidateCaches()
		delay := streak.record(time.Since(started))
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
	}
}

func (s *Session) eventLoop(stop <-chan struct{}, state *eventState, notify func()) error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			cancel()
		case <-done:
		}
	}()
	defer cancel()
	conn, _, err := websocket.Dial(ctx, s.eventURL(state), nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		typ, payload, ok := strings.Cut(string(data), ";")
		if !ok || typ != "entityUpdate" {
			continue
		}
		raw := instance{}
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			continue
		}
		if owner := raw.str("1486"); owner != "" {
			if bid := raw.str("1485"); bid != "" {
				state.noteBatch(owner, bid)
			}
		}
		relevant := false
		for _, u := range raw.aggList("1487") {
			if u.str("464") == "tutanota" {
				switch u.str("2556") {
				case "97":
					relevant = true
					if id := u.str("467"); id != "" {
						s.markMailDirty(id)
					}
				case "429":
					relevant = true
					s.markFoldersStale()
				case "1450":
					relevant = true
					if id := u.str("466"); id != "" {
						s.markEntriesStale(id)
					}
				}
			}
		}
		if relevant {
			notify()
		}
	}
}
