package tuta

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type eventState struct {
	lastBatchByGroup map[string]string
}

func (s *Session) eventURL(state *eventState) string {
	q := url.Values{}
	q.Set("modelVersions", modelVersionSys+"."+modelVersionTutanota)
	q.Set("clientVersion", clientVersion)
	q.Set("userId", s.userID)
	q.Set("accessToken", s.accessToken)
	if len(state.lastBatchByGroup) > 0 {
		parts := make([]string, 0, len(state.lastBatchByGroup))
		for group, batch := range state.lastBatchByGroup {
			parts = append(parts, group+"="+batch)
		}
		q.Set("groupsToLastEventBatchIds", strings.Join(parts, ";"))
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

func (s *Session) WatchEvents(stop <-chan struct{}, notify func()) {
	state := &eventState{lastBatchByGroup: make(map[string]string)}
	failures := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		err := s.eventLoop(stop, state, notify)
		if err != nil {
			if code := websocket.CloseStatus(err); code == 4401 || code == 4470 || code == 4472 {
				return
			}
		}
		failures++
		select {
		case <-stop:
			return
		case <-time.After(reconnectDelay(failures)):
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
				state.lastBatchByGroup[owner] = bid
			}
		}
		relevant := false
		for _, u := range raw.aggList("1487") {
			if u.str("464") == "tutanota" {
				switch u.str("2556") {
				case "97", "429", "1450":
					relevant = true
				}
			}
		}
		if relevant {
			notify()
		}
	}
}
