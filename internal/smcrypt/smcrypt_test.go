package smcrypt

import (
	"encoding/hex"
	"testing"
)

const testSalt = "vectortest-salt-0123456789abcdef"

func TestDecryptUrlSafeStringKey(t *testing.T) {
	v, err := DecryptUrlSafe("eHh0ZWE.s6474i877KbLp2xiX5KIpQ.28cILUe0n-gTRtylXFK50pppUSgQogaN", []byte("mailpass-xyz"), testSalt)
	if err != nil {
		t.Fatal(err)
	}
	if v != "hello-namakani-123" {
		t.Fatalf("got %q", v)
	}
}

func TestDecryptFromJSONSameEnvelope(t *testing.T) {
	v, err := DecryptFromJSON(`["eHh0ZWE=","s6474i877KbLp2xiX5KIpQ==","28cILUe0n+gTRtylXFK50pppUSgQogaN"]`, []byte("mailpass-xyz"), testSalt)
	if err != nil {
		t.Fatal(err)
	}
	if v != "hello-namakani-123" {
		t.Fatalf("got %q", v)
	}
}

func TestDecryptUrlSafeBinaryKey(t *testing.T) {
	key, _ := hex.DecodeString("0123456789abcdef0123456789abcdef01234567")
	v, err := DecryptUrlSafe("eHh0ZWE.6RiDxgxHPSFE_lvqSKLr1g.8WxL2KPxGIdobt24DNlG7ONU1z6mkPbMe9CCM_KQP8Q", key, testSalt)
	if err != nil {
		t.Fatal(err)
	}
	if v != "p@ss word/with:symbols" {
		t.Fatalf("got %q", v)
	}
}

func TestKeyIndependenceOfXxteaEnvelope(t *testing.T) {
	v, err := DecryptUrlSafe("eHh0ZWE.s6474i877KbLp2xiX5KIpQ.28cILUe0n-gTRtylXFK50pppUSgQogaN", []byte("wrong-password"), testSalt)
	if err != nil {
		t.Fatal(err)
	}
	if v != "hello-namakani-123" {
		t.Fatalf("got %q", v)
	}
}
