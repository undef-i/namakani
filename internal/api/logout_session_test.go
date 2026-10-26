package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"namakani/internal/auth"
	"namakani/internal/mail"
)

func sessionReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.AddCookie(&http.Cookie{Name: "namakani_session", Value: token})
	return req
}

func (f *drainFixture) liveSession(t *testing.T) (string, []byte) {
	t.Helper()
	token, err := auth.NewSession(f.st, "")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	f.srv.keys[token] = key
	return token, key
}

func TestCredentialKeyRejectsDeletedSession(t *testing.T) {
	f := newDrainFixture(t)
	token, _ := f.liveSession(t)
	if _, ok := f.srv.credentialKey(sessionReq(token)); !ok {
		t.Fatal("live session must authenticate")
	}
	if err := auth.DeleteSession(f.st, token); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, ok := f.srv.credentialKey(sessionReq(token)); ok {
		t.Fatal("a deleted session must stop authenticating immediately, not at the next restart")
	}
}

func TestCredentialKeyRejectsExpiredSession(t *testing.T) {
	f := newDrainFixture(t)
	token, _ := f.liveSession(t)
	if _, err := f.st.DB.Exec("UPDATE sessions SET expires_at = ?", time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, ok := f.srv.credentialKey(sessionReq(token)); ok {
		t.Fatal("an expired session must stop authenticating even while its key is still cached in memory")
	}
}

func TestLogoutStopsWorkersOnlyWhenLastSessionEnds(t *testing.T) {
	f := newDrainFixture(t)
	tokenA, _ := f.liveSession(t)
	tokenB, _ := f.liveSession(t)
	f.srv.mgr.Start(mail.Account{ID: 1, Email: "a@x.test"})
	if got := f.srv.mgr.Status(1).State; got == "stopped" {
		t.Fatalf("worker state = %q before any logout, want a live worker", got)
	}
	rec := httptest.NewRecorder()
	f.srv.handleLogout(rec, sessionReq(tokenA))
	if got := f.srv.mgr.Status(1).State; got == "stopped" {
		t.Fatal("logging out one device stopped the account workers while another session is still live")
	}
	if _, ok := f.srv.credentialKey(sessionReq(tokenB)); !ok {
		t.Fatal("the other device's session must keep working after this logout")
	}
	if _, ok := f.srv.credentialKey(sessionReq(tokenA)); ok {
		t.Fatal("the logged-out session must stop working")
	}
	rec = httptest.NewRecorder()
	f.srv.handleLogout(rec, sessionReq(tokenB))
	if got := f.srv.mgr.Status(1).State; got != "stopped" {
		t.Fatalf("worker state = %q after the last session logged out, want stopped", got)
	}
}
