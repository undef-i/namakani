package tuta

import (
	"testing"
	"time"
)

func TestRestClientTimeoutBudget(t *testing.T) {
	if clientTimeout != 90*time.Second {
		t.Fatalf("clientTimeout = %v, want 90s", clientTimeout)
	}
	if got := newRestClient().http.Timeout; got != clientTimeout {
		t.Fatalf("rest client Timeout = %v, want %v", got, clientTimeout)
	}
}
