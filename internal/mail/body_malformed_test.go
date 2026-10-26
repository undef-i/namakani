package mail

import (
	"strings"
	"testing"
)

func TestParseBodyUnknownCharsetDegradesInsteadOfRefusing(t *testing.T) {
	raw := "From: a@x.test\r\nTo: b@x.test\r\nSubject: t\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=\"x-bogus-charset\"\r\n\r\nhello body\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"a.bin\"\r\n\r\naGVsbG8=\r\n" +
		"--B--\r\n"
	parsed, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody must not refuse the whole message over one bad part: %v", err)
	}
	if !parsed.Truncated {
		t.Fatal("a part that failed to decode must mark the body truncated")
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != "a.bin" {
		t.Fatalf("attachments = %+v, want the intact a.bin part to survive", parsed.Attachments)
	}
}

func TestParseBodyCorruptAttachmentKeepsText(t *testing.T) {
	raw := "From: a@x.test\r\nTo: b@x.test\r\nSubject: t\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nthe good text\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"bad.bin\"\r\n\r\n!!!not*base64!!!\r\n" +
		"--B--\r\n"
	parsed, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if parsed.Text != "the good text" {
		t.Fatalf("text = %q, want the intact text part preserved", parsed.Text)
	}
	if !parsed.Truncated {
		t.Fatal("a corrupt attachment must mark the body truncated")
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("attachments = %d, want the corrupt part kept as a placeholder row", len(parsed.Attachments))
	}
}

func TestParseBodyMissingBoundaryFallsBackToRaw(t *testing.T) {
	raw := "From: a@x.test\r\nTo: b@x.test\r\nSubject: lost body\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"NOPE\"\r\n\r\n" +
		"--REAL\r\nContent-Type: text/plain\r\n\r\nthe payload that must not vanish\r\n--REAL--\r\n"
	parsed, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if !strings.Contains(parsed.Text, "the payload that must not vanish") {
		t.Fatalf("text = %q, want a raw fallback that keeps the payload readable instead of an empty body", parsed.Text)
	}
	if !parsed.Truncated {
		t.Fatal("the raw fallback must be marked truncated")
	}
}

func TestParseBodyMalformedMidPartHeaderKeepsEarlierParts(t *testing.T) {
	raw := "From: a@x.test\r\nTo: b@x.test\r\nSubject: t\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nfirst part text\r\n" +
		"--B\r\nthis header line has no colon\r\n\r\nsecond part\r\n" +
		"--B--\r\n"
	parsed, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if parsed.Text != "first part text" {
		t.Fatalf("text = %q, want the intact first part", parsed.Text)
	}
	if !parsed.Truncated {
		t.Fatal("a part lost to a malformed header must mark the body truncated")
	}
}

func TestParseBodyWellFormedIsNotTruncated(t *testing.T) {
	raw := "From: a@x.test\r\nTo: b@x.test\r\nSubject: t\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nclean text\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"ok.bin\"\r\n\r\naGVsbG8=\r\n" +
		"--B--\r\n"
	parsed, err := ParseBody([]byte(raw))
	if err != nil {
		t.Fatalf("ParseBody: %v", err)
	}
	if parsed.Truncated {
		t.Fatal("a well-formed message must not be marked truncated")
	}
	if parsed.Text != "clean text" || len(parsed.Attachments) != 1 {
		t.Fatalf("parsed = text %q, %d attachments; want clean text and 1 attachment", parsed.Text, len(parsed.Attachments))
	}
	att, err := ParseBodyPart([]byte(raw), parsed.Attachments[0].PartPath)
	if err != nil {
		t.Fatalf("ParseBodyPart: %v", err)
	}
	if string(att.Bytes) != "hello" {
		t.Fatalf("attachment bytes = %q, want hello", att.Bytes)
	}
}
