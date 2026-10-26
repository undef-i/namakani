package sync

import (
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"namakani/internal/mail"
)

func TestClassifySessionErrorReasonOmitsRawError(t *testing.T) {
	raw := &imap.Error{Code: imap.ResponseCodeAuthenticationFailed, Text: "RAW-MARKER server banner detail"}
	term, reason := classifySessionError(mail.Account{Kind: "imap"}, raw)
	if !term {
		t.Fatalf("auth failure must be terminal")
	}
	if strings.Contains(reason, "RAW-MARKER") {
		t.Fatalf("user-facing reason %q embeds the raw upstream error; the raw text belongs in logs, not in the reconnect instructions", reason)
	}
	if !strings.Contains(reason, "重新连接") {
		t.Fatalf("reason %q lost its actionable instruction", reason)
	}
}
