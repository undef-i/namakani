package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 20*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 20s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 90*time.Second {
		t.Fatalf("IdleTimeout = %v, want 90s", srv.IdleTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, want 0 (SSE and large uploads must not be cut)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0 (SSE streams are long-lived)", srv.WriteTimeout)
	}
}
