package tuta

import (
	"errors"
	"testing"

	"github.com/coder/websocket"
)

func TestTerminalCloseCode(t *testing.T) {
	for _, code := range []int{4401, 4470, 4472} {
		err := websocket.CloseError{Code: websocket.StatusCode(code)}
		if got, ok := terminalCloseCode(err); !ok || got != code {
			t.Fatalf("close %d: got (%d, %v), want (%d, true): an auth-class close must end the watch so the account can be parked for repair", code, got, ok, code)
		}
	}
	for _, err := range []error{
		websocket.CloseError{Code: websocket.StatusGoingAway},
		websocket.CloseError{Code: websocket.StatusAbnormalClosure},
		errors.New("connection reset"),
		nil,
	} {
		if _, ok := terminalCloseCode(err); ok {
			t.Fatalf("err %v classified terminal, want retryable", err)
		}
	}
}
