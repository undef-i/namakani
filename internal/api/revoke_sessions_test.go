package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func loginCookie(t *testing.T, f *tokenFixture) *http.Cookie {
	t.Helper()
	rec := f.do(f.srv, http.MethodPost, "/api/login", `{"password":"test-master-123"}`, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "namakani_session" {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestRevokeAllSessions(t *testing.T) {
	f := newTokenFixture(t)
	second := loginCookie(t, f)
	_, bearer := f.createToken(t, f.srv, "dev")

	if rec := f.do(f.srv, http.MethodPost, "/api/settings/sessions/revoke-all", `{}`, nil, bearer); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoke-all with only an access token: status %d, want 401 (sessions are revoked from a session)", rec.Code)
	}

	rec := f.do(f.srv, http.MethodPost, "/api/settings/sessions/revoke-all", `{}`, f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-all: status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Revoked int64 `json:"revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode revoke-all response: %v", err)
	}
	if out.Revoked != 2 {
		t.Fatalf("revoked = %d, want 2 (both logins)", out.Revoked)
	}

	for name, cookie := range map[string]*http.Cookie{"caller": f.cookie, "other": second} {
		if rec := f.do(f.srv, http.MethodGet, "/api/accounts", "", cookie, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s session after revoke-all: status %d, want 401", name, rec.Code)
		}
	}
	if rec := f.do(f.srv, http.MethodGet, "/api/accounts", "", nil, bearer); rec.Code != http.StatusOK {
		t.Fatalf("access token after revoke-all: status %d, want 200 (tokens are a separate table)", rec.Code)
	}
}
