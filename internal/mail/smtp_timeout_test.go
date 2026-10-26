package mail

import (
	"net"
	"testing"
	"time"
)

func TestSMTPTimeoutBudget(t *testing.T) {
	if smtpTimeout != 90*time.Second {
		t.Fatalf("smtpTimeout = %v, want 90s", smtpTimeout)
	}
}

// A server that accepts the connection and then never speaks must not
// hang the send path: the connection deadline has to fire.
func TestSMTPStalledServerTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			buf := make([]byte, 1024)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	old := smtpTimeout
	smtpTimeout = 300 * time.Millisecond
	defer func() { smtpTimeout = old }()

	acct := Account{
		SMTPHost: "127.0.0.1",
		SMTPPort: port,
		SMTPTLS:  "none",
		Username: "u@x.test",
		Password: "secret",
	}
	start := time.Now()
	err = TestSMTP(acct)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("stalled SMTP server: TestSMTP = nil, want a timeout error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("stalled SMTP server: gave up after %v, want the 300ms deadline to fire promptly", elapsed)
	}
}
