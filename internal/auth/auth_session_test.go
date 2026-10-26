package auth

import (
	"path/filepath"
	"testing"
	"time"

	"namakani/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	return st
}

func sessionExpiry(t *testing.T, st *store.Store, token string) int64 {
	t.Helper()
	_, expiresAt, exists, err := SessionInfo(st, token)
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	if !exists {
		t.Fatal("session not found")
	}
	return expiresAt
}

func backdateSession(t *testing.T, st *store.Store, token string, createdAt, expiresAt int64) {
	t.Helper()
	if _, err := st.DB.Exec("UPDATE sessions SET created_at = ?, expires_at = ? WHERE token_hash = ?",
		createdAt, expiresAt, tokenHash(token)); err != nil {
		t.Fatalf("backdate session: %v", err)
	}
}

// Sliding renewal must never push a session past first-login + SessionTTL,
// and a legacy expiry already past that cap is clamped down on touch.
func TestTouchSessionAbsoluteCap(t *testing.T) {
	st := openTestStore(t)
	ttl := int64(SessionTTL / time.Second)
	now := time.Now().Unix()
	created := now - 300*24*3600

	renew, err := NewSession(st, "wrapped")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	backdateSession(t, st, renew, created, created+350*24*3600)
	if err := TouchSession(st, renew); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if got, want := sessionExpiry(t, st, renew), created+ttl; got != want {
		t.Fatalf("renewal expiry = %d, want absolute cap %d (first login + TTL)", got, want)
	}

	over, err := NewSession(st, "wrapped")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	backdateSession(t, st, over, created, now+350*24*3600)
	if err := TouchSession(st, over); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if got, want := sessionExpiry(t, st, over), created+ttl; got != want {
		t.Fatalf("over-cap legacy expiry = %d, want clamped to %d", got, want)
	}
}

// A session still inside the renewal threshold (remaining life above
// SessionTTL-24h, i.e. touched within its first day) is not rewritten.

func TestDeleteAllSessionsKeepsAccessTokens(t *testing.T) {
	st := openTestStore(t)
	var tokens []string
	for i := 0; i < 3; i++ {
		tok, err := NewSession(st, "wrapped")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		tokens = append(tokens, tok)
	}
	_, plain, err := NewAccessToken(st, "dev", "wrapped")
	if err != nil {
		t.Fatalf("NewAccessToken: %v", err)
	}
	n, err := DeleteAllSessions(st)
	if err != nil {
		t.Fatalf("DeleteAllSessions: %v", err)
	}
	if n != 3 {
		t.Fatalf("DeleteAllSessions removed %d rows, want 3", n)
	}
	for _, tok := range tokens {
		if _, _, exists, err := SessionInfo(st, tok); err != nil || exists {
			t.Fatalf("session still resolves after revoke-all: exists=%v err=%v", exists, err)
		}
	}
	if _, _, exists, err := AccessTokenInfo(st, plain); err != nil || !exists {
		t.Fatalf("access token lost in revoke-all: exists=%v err=%v", exists, err)
	}
	if live, err := LiveSessionCount(st); err != nil || live != 0 {
		t.Fatalf("LiveSessionCount = %d, %v; want 0", live, err)
	}
}

// (Re)setting the master password invalidates every session: their
// wrapped keys belong to the old key and can never be used again.
func TestSetupMasterRevokesSessions(t *testing.T) {
	st := openTestStore(t)
	if err := SetupMaster(st, "first-password-1"); err != nil {
		t.Fatalf("SetupMaster: %v", err)
	}
	tok, err := NewSession(st, "wrapped")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := SetupMaster(st, "second-password-2"); err != nil {
		t.Fatalf("SetupMaster again: %v", err)
	}
	if _, _, exists, err := SessionInfo(st, tok); err != nil || exists {
		t.Fatalf("session survived a master password change: exists=%v err=%v", exists, err)
	}
	if _, ok, err := VerifyMaster(st, "second-password-2"); err != nil || !ok {
		t.Fatalf("VerifyMaster with new password: ok=%v err=%v", ok, err)
	}
}
