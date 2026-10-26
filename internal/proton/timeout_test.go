package proton

import (
	"net/http"
	"testing"
	"time"
)

func TestClientTimeoutBudget(t *testing.T) {
	if clientTimeout != 90*time.Second {
		t.Fatalf("clientTimeout = %v, want 90s", clientTimeout)
	}
	if got := newClient().HTTPClient.Timeout; got != clientTimeout {
		t.Fatalf("newClient HTTPClient.Timeout = %v, want %v", got, clientTimeout)
	}
	if got := newHTTPClient(http.DefaultTransport).Timeout; got != clientTimeout {
		t.Fatalf("newHTTPClient Timeout = %v, want %v", got, clientTimeout)
	}
}
