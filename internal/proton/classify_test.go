package proton

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyErrorReasonOmitsRawError(t *testing.T) {
	term, reason := ClassifyError(errors.New("two factor authentication enabled: RAW-MARKER upstream detail"))
	if !term {
		t.Fatalf("two factor error must be terminal")
	}
	if strings.Contains(reason, "RAW-MARKER") {
		t.Fatalf("user-facing reason %q embeds the raw upstream error", reason)
	}
	if !strings.Contains(reason, "重新连接") {
		t.Fatalf("reason %q lost its actionable instruction", reason)
	}
}
