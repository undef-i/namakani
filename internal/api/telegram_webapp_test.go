package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"namakani/internal/auth"
	"namakani/internal/store"
)

const tgTestToken = "123456:TESTTOKEN-for-webapp-auth-unit-tests"

const tgVectorA = "auth_date=1760000000&query_id=AAHdF6IQAAAAAN0XohDhrOrc&user=%7B%22id%22%3A424242%2C%22first_name%22%3A%22Tester%22%2C%22username%22%3A%22tester%22%7D&hash=faaed2e52d7f93bc0c87d80783552d2d1b129f193a2a54a7965d1252efc08f67"

const tgVectorCold = "auth_date=1700000000&query_id=AAHdF6IQAAAAAN0XohDhrOrc&user=%7B%22id%22%3A424242%2C%22first_name%22%3A%22Tester%22%2C%22username%22%3A%22tester%22%7D&hash=71b286c9da502b50cf83323ca76efa6ed2146ced46f17c50d968c3654faa4b14"

func TestVerifyTGInitDataExternalVector(t *testing.T) {
	now := time.Unix(1760000000+3600, 0)
	got, err := verifyTGInitData(tgTestToken, tgVectorA, now)
	if err != nil {
		t.Fatalf("valid external vector rejected: %v", err)
	}
	if got.AuthDate != 1760000000 {
		t.Fatalf("auth_date = %d, want 1760000000", got.AuthDate)
	}
}

func TestVerifyTGInitDataTampered(t *testing.T) {
	now := time.Unix(1760000000+3600, 0)
	tampered := strings.Replace(tgVectorA, "Tester", "Hacker", 1)
	if _, err := verifyTGInitData(tgTestToken, tampered, now); err == nil {
		t.Fatal("tampered init data accepted")
	}
	badHash := strings.Replace(tgVectorA, "hash=faae", "hash=0000", 1)
	if _, err := verifyTGInitData(tgTestToken, badHash, now); err == nil {
		t.Fatal("init data with wrong hash accepted")
	}
	if _, err := verifyTGInitData("654321:OTHERTOKEN", tgVectorA, now); err == nil {
		t.Fatal("init data verified against the wrong bot token")
	}
}

func TestVerifyTGInitDataExpired(t *testing.T) {
	now := time.Unix(1700000000, 0).Add(25 * time.Hour)
	if _, err := verifyTGInitData(tgTestToken, tgVectorCold, now); err == nil {
		t.Fatal("stale init data accepted")
	}
	fresh := time.Unix(1700000000+600, 0)
	if _, err := verifyTGInitData(tgTestToken, tgVectorCold, fresh); err != nil {
		t.Fatalf("init data inside the window rejected: %v", err)
	}
}

func signTGInitData(token string, userID int64, authDate int64) string {
	user := fmt.Sprintf(`{"id":%d,"first_name":"Tester","username":"tester"}`, userID)
	pairs := map[string]string{
		"auth_date": fmt.Sprint(authDate),
		"query_id":  "AAHdF6IQAAAAAN0XohDhrOrc",
		"user":      user,
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+pairs[k])
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(parts, "\n")))
	pairs["hash"] = hex.EncodeToString(mac.Sum(nil))
	vals := url.Values{}
	for k, v := range pairs {
		vals.Set(k, v)
	}
	return vals.Encode()
}

func newTGTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	serverKey := make([]byte, 32)
	for i := range serverKey {
		serverKey[i] = byte(i + 1)
	}
	enc, err := auth.Encrypt(serverKey, tgTestToken)
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if err := st.SetSetting(settingTGTokenEnc, enc); err != nil {
		t.Fatalf("store token: %v", err)
	}
	if err := st.SetSetting(settingTGChatID, "424242"); err != nil {
		t.Fatalf("store chat id: %v", err)
	}
	wrapped, err := auth.Encrypt(serverKey, "the-credential-key")
	if err != nil {
		t.Fatalf("encrypt credential key: %v", err)
	}
	if _, err := auth.NewSession(st, wrapped); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return &Server{
		st:        st,
		serverKey: serverKey,
		keys:      make(map[string][]byte),
		tgSeen:    make(map[string]tgChat),
	}
}

func postInitData(t *testing.T, s *Server, initData string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"init_data": initData})
	req := httptest.NewRequest(http.MethodPost, "/api/telegram/webapp-auth", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleTelegramWebAppAuth(rec, req)
	return rec
}

func TestHandleTelegramWebAppAuthBoundUser(t *testing.T) {
	s := newTGTestServer(t)
	initData := signTGInitData(tgTestToken, 424242, time.Now().Unix())
	rec := postInitData(t, s, initData)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Header().Values("Set-Cookie")
	found := false
	for _, c := range cookies {
		if strings.HasPrefix(c, "namakani_session=") && !strings.HasPrefix(c, "namakani_session=;") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session cookie issued, headers = %v", rec.Header())
	}
}

func TestHandleTelegramWebAppAuthWrongUser(t *testing.T) {
	s := newTGTestServer(t)
	initData := signTGInitData(tgTestToken, 999999, time.Now().Unix())
	rec := postInitData(t, s, initData)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleTelegramWebAppAuthForged(t *testing.T) {
	s := newTGTestServer(t)
	initData := signTGInitData(tgTestToken, 424242, time.Now().Unix())
	forged := strings.Replace(initData, "424242", "424243", 1)
	rec := postInitData(t, s, forged)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if rec := postInitData(t, s, "not-a-query-string"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("garbage init data status = %d, want 401", rec.Code)
	}
}
