package mail

import (
	"strings"
	"testing"
)

func TestParseBodyCappedMarksTruncation(t *testing.T) {
	raw := "From: A <a@x.test>\r\nTo: B <b@x.test>\r\nSubject: big\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.Repeat("body line\r\n", 500)
	p, err := ParseBodyCapped([]byte(raw), 1000)
	if err != nil {
		t.Fatalf("ParseBodyCapped: %v", err)
	}
	if !p.Truncated {
		t.Fatal("oversize body: Truncated = false, want true")
	}
	if len(p.Text) > 1000 {
		t.Fatalf("oversize body: stored text is %d bytes, want <= 1000", len(p.Text))
	}
	if p.Text == "" {
		t.Fatal("oversize body: stored text is empty, want the kept prefix")
	}
}

func TestParseBodyCappedUnderLimitUntouched(t *testing.T) {
	raw := "From: A <a@x.test>\r\nSubject: small\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n"
	for _, limit := range []int64{1000, 0} {
		p, err := ParseBodyCapped([]byte(raw), limit)
		if err != nil {
			t.Fatalf("ParseBodyCapped(limit=%d): %v", limit, err)
		}
		if p.Truncated {
			t.Fatalf("limit=%d: Truncated = true, want false", limit)
		}
		if !strings.Contains(p.Text, "hello") {
			t.Fatalf("limit=%d: text = %q, want it to contain hello", limit, p.Text)
		}
	}
}

func TestCapBuffer(t *testing.T) {
	cb := &CapBuffer{Limit: 10}
	if _, err := cb.Write([]byte("12345")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if cb.Overflow() {
		t.Fatal("Overflow = true after 5 of 10 bytes, want false")
	}
	n, err := cb.Write([]byte("12345678"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 8 {
		t.Fatalf("write consumed %d bytes, want 8 (overflow is discarded, not an error)", n)
	}
	if !cb.Overflow() {
		t.Fatal("Overflow = false after exceeding the cap, want true")
	}
	if got := string(cb.Bytes()); got != "1234512345" {
		t.Fatalf("Bytes = %q, want the first 10 bytes %q", got, "1234512345")
	}
}
