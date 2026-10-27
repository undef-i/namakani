package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testCreds() Creds {
	return Creds{ClientID: "cid-1", ClientSecret: "secret-1", RefreshToken: "rt-1"}
}

func TestTokenRefreshLifecycle(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	c := NewClient(testCreds())
	ctx := context.Background()
	if _, err := c.Profile(ctx); err != nil {
		t.Fatalf("profile: %v", err)
	}
	if n := f.tokenCallCount(); n != 1 {
		t.Fatalf("token calls = %d; want 1 for the first API call", n)
	}
	if _, err := c.Profile(ctx); err != nil {
		t.Fatalf("profile 2: %v", err)
	}
	if n := f.tokenCallCount(); n != 1 {
		t.Fatalf("token calls = %d; want still 1 while the access token is valid", n)
	}
	base := time.Now()
	c.now = func() time.Time { return base.Add(2 * time.Hour) }
	if _, err := c.Profile(ctx); err != nil {
		t.Fatalf("profile after expiry: %v", err)
	}
	if n := f.tokenCallCount(); n != 2 {
		t.Fatalf("token calls = %d; want a refresh after expiry", n)
	}
	f.mu.Lock()
	f.rotateTo = "rt-2"
	f.mu.Unlock()
	c.now = func() time.Time { return base.Add(4 * time.Hour) }
	if _, err := c.Profile(ctx); err != nil {
		t.Fatalf("profile after second expiry: %v", err)
	}
	if got := c.Creds().RefreshToken; got != "rt-2" {
		t.Fatalf("refresh token after rotation = %q; want rt-2", got)
	}
}

func TestRevokedRefreshTokenIsAuthError(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	f.mu.Lock()
	f.refreshToken = "rt-other"
	f.mu.Unlock()
	c := NewClient(testCreds())
	_, err := c.Profile(context.Background())
	if err == nil {
		t.Fatal("profile with a revoked refresh token succeeded; want an auth error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T; want APIError", err)
	}
	if !IsAuthError(err) {
		t.Fatalf("IsAuthError(%v) = false; want true", err)
	}
}

func TestCredsJSONRoundTrip(t *testing.T) {
	in := testCreds()
	out, err := ParseCreds(in.JSON())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out != in {
		t.Fatalf("round trip = %+v; want %+v", out, in)
	}
	if _, err := ParseCreds("not json"); err == nil {
		t.Fatal("ParseCreds accepted garbage")
	}
	if _, err := ParseCreds(`{"client_id":"x"}`); err == nil {
		t.Fatal("ParseCreds accepted creds without a refresh token")
	}
}

func TestAuthCodeURLCarriesPKCEAndOfflineAccess(t *testing.T) {
	raw := AuthCodeURL("cid-1", "https://mail.example.test/api/gmail/callback", "state-9", "chal-9", "user@gmail.com")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if u.Scheme != "https" || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("auth url base = %s://%s%s; want the Google consent endpoint", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             "cid-1",
		"redirect_uri":          "https://mail.example.test/api/gmail/callback",
		"response_type":         "code",
		"scope":                 "https://mail.google.com/",
		"access_type":           "offline",
		"prompt":                "consent",
		"state":                 "state-9",
		"code_challenge":        "chal-9",
		"code_challenge_method": "S256",
		"login_hint":            "user@gmail.com",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("auth url param %s = %q; want %q (url %s)", k, q.Get(k), v, raw)
		}
	}
}

func TestPKCEPairVerifies(t *testing.T) {
	verifier, challenge, err := PKCEPair()
	if err != nil {
		t.Fatalf("pkce: %v", err)
	}
	if len(verifier) != 43 {
		t.Fatalf("verifier length = %d; want 43 (32 random bytes, base64url)", len(verifier))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Fatalf("challenge = %q; want S256 of verifier %q", challenge, want)
	}
	v2, _, err := PKCEPair()
	if err != nil || v2 == verifier {
		t.Fatalf("two PKCE pairs collide or error: %v", err)
	}
	s1, err := RandomState()
	if err != nil || len(s1) < 32 {
		t.Fatalf("state = %q, %v; want at least 32 chars", s1, err)
	}
	s2, _ := RandomState()
	if s1 == s2 {
		t.Fatal("two states collide")
	}
}

func TestExchangeCodeAgainstFakeTokenEndpoint(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	creds, err := ExchangeCode(context.Background(), "cid-1", "secret-1", "good-code", "verifier-1", "https://mail.example.test/api/gmail/callback")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if creds.RefreshToken != "rt-1" || creds.ClientID != "cid-1" || creds.ClientSecret != "secret-1" {
		t.Fatalf("exchanged creds = %+v; want the client pair plus rt-1", creds)
	}
	if _, err := ExchangeCode(context.Background(), "cid-1", "secret-1", "bad-code", "verifier-1", "https://mail.example.test/api/gmail/callback"); err == nil {
		t.Fatal("exchange with a bad code succeeded; want an error")
	}
	if !f.codeRedeemed {
		t.Fatal("fake never saw a successful code redemption")
	}
	_ = strings.TrimSpace
}
