package tuta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestEventReadLoopSurvivesOversizedFrameAndNotifies(t *testing.T) {
	big := "phishingMarkers;" + strings.Repeat("x", 40000)
	relevant := `entityUpdate;{"1485":"B1","1486":"G1","1487":[{"464":"tutanota","2556":"97","466":"L1","467":"M1"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		if err := c.Write(ctx, websocket.MessageText, []byte(big)); err != nil {
			return
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(relevant)); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	s := &Session{}
	state := &eventState{committed: map[string]string{}, pending: map[string]string{}}
	notified := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- s.eventReadLoop(ctx, conn, state, func() { notified <- struct{}{} }) }()

	select {
	case <-notified:
	case <-time.After(10 * time.Second):
		t.Fatal("no notification from the relevant frame")
	}
	if mark := state.mark(); mark["G1"] != "B1" {
		t.Fatalf("mark = %v", mark)
	}
	if dirty := s.peekDirtyMails(); !dirty["M1"] {
		t.Fatalf("dirty = %v", dirty)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read loop ended without error after cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read loop did not end after cancel")
	}
}
