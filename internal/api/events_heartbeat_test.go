package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEventsHeartbeatIsADataFrameTheClientCanObserve(t *testing.T) {
	prev := eventsHeartbeatInterval
	eventsHeartbeatInterval = 30 * time.Millisecond
	t.Cleanup(func() { eventsHeartbeatInterval = prev })
	f := newGmailFixture(t)
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "namakani_session", Value: f.token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d; want 200", resp.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		r := bufio.NewReader(resp.Body)
		var sb strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			sb.WriteString(line)
			if line == "\n" {
				frames <- sb.String()
				sb.Reset()
			}
		}
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("event stream closed before any heartbeat data frame arrived: a heartbeat the client cannot observe cannot prove the stream is alive")
			}
			var data string
			for _, line := range strings.Split(frame, "\n") {
				if strings.HasPrefix(line, "data: ") {
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			if data == "" {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("event data is not JSON: %v", err)
			}
			if ev["type"] != "ping" {
				continue
			}
			if _, hasSeq := ev["seq"]; hasSeq {
				t.Fatal("heartbeat carries a sequence number: it must stay out of the broadcast sequence space or gap detection will misfire")
			}
			return
		case <-deadline:
			t.Fatal("no heartbeat data frame arrived within 5s at a 30ms heartbeat interval: the heartbeat must be an event the client can observe, not a stream comment")
		}
	}
}
