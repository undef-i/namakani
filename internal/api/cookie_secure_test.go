package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"namakani/internal/auth"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

func loginForCookie(t *testing.T, baseURL, forwardedProto string) *http.Cookie {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if err := auth.SetupMaster(st, "test-master-123"); err != nil {
		t.Fatalf("setup master: %v", err)
	}
	if baseURL != "" {
		cfg := st.AppSettings()
		cfg.BaseURL = baseURL
		if err := st.SaveAppSettings(cfg); err != nil {
			t.Fatalf("save settings: %v", err)
		}
	}
	srv := New(st, nil, syncmgr.NewManager(st))
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"test-master-123"}`))
	if forwardedProto != "" {
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "namakani_session" {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestLoginCookieSecureWhenBaseURLIsHTTPS(t *testing.T) {
	// Production chain: Cloudflare reaches lighttpd over plain HTTP and
	// lighttpd rewrites X-Forwarded-Proto to http, so the request itself
	// looks like HTTP although the public entry is the https base_url.
	c := loginForCookie(t, "https://mail.example.com", "http")
	if !c.Secure {
		t.Fatal("session cookie lacks Secure although base_url declares https and the request only looks like http because of the proxy chain")
	}
}

func TestLoginCookiePlainHTTPSchemes(t *testing.T) {
	if c := loginForCookie(t, "", "http"); c.Secure {
		t.Fatal("session cookie carries Secure on a plain http deployment without an https base_url")
	}
	if c := loginForCookie(t, "http://mail.example.com", "http"); c.Secure {
		t.Fatal("session cookie carries Secure although base_url declares http")
	}
	if c := loginForCookie(t, "", "https"); !c.Secure {
		t.Fatal("session cookie lacks Secure on a direct https request")
	}
}
