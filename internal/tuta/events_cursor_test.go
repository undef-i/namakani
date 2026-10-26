package tuta

import (
	"strings"
	"testing"
)

func TestEventCursorOnlyAdvancesOnAck(t *testing.T) {
	s := &eventState{committed: map[string]string{}, pending: map[string]string{}}
	s.noteBatch("g1", "b1")
	s.noteBatch("g2", "b1")
	mark := s.mark()
	s.noteBatch("g1", "b2")
	s.ack(mark)
	url := s.connectParams()
	if strings.Contains(url, "g1=b2") {
		t.Fatal("a batch that arrived during the sync must not be committed by that sync's ack")
	}
	if strings.Contains(url, "g1=b1") {
		t.Fatal("a superseded batch must not be committed either; g1 stays uncommitted until a later ack")
	}
	if !strings.Contains(url, "g2=b1") {
		t.Fatalf("connect params = %q, want g2=b1 committed", url)
	}
	s.ack(s.mark())
	url = s.connectParams()
	if !strings.Contains(url, "g1=b2") {
		t.Fatalf("connect params = %q, want g1=b2 committed after a full ack", url)
	}
}

func TestEventCursorWithoutAckReplaysEverything(t *testing.T) {
	s := &eventState{committed: map[string]string{}, pending: map[string]string{}}
	s.noteBatch("g1", "b9")
	if url := s.connectParams(); strings.Contains(url, "g1") {
		t.Fatalf("connect params = %q, want no cursor for an unacked batch so the server replays it after a reconnect", url)
	}
}
