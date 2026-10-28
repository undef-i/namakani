package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"namakani/internal/auth"
	"namakani/internal/gmail"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

type apiFakeGmail struct {
	mu        sync.Mutex
	sendRaws  []string
	tokenHits int
	srv       *httptest.Server
}

func newAPIFakeGmail(t *testing.T) *apiFakeGmail {
	t.Helper()
	f := &apiFakeGmail{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenHits++
		f.mu.Unlock()
		if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "bad-rt" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "bad refresh token"})
			return
		}
		resp := map[string]any{"access_token": "at-1", "expires_in": 3600, "token_type": "Bearer"}
		if r.Form.Get("grant_type") == "authorization_code" {
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "bad code"})
				return
			}
			resp["refresh_token"] = "rt-1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/gmail/v1/users/me/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": "user@gmail.com", "messagesTotal": 0, "historyId": "1000"})
	})
	mux.HandleFunc("/gmail/v1/users/me/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
			{"id": "INBOX", "name": "INBOX", "type": "system"},
			{"id": "SENT", "name": "SENT", "type": "system"},
		}})
	})
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resultSizeEstimate": 0})
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Raw string `json:"raw"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sendRaws = append(f.sendRaws, body.Raw)
		n := len(f.sendRaws)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("sent-%d", n), "labelIds": []string{"SENT"}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	oldAPI, oldToken := gmail.APIBase, gmail.TokenURL
	gmail.APIBase = f.srv.URL + "/gmail/v1/users/me"
	gmail.TokenURL = f.srv.URL + "/token"
	t.Cleanup(func() { gmail.APIBase, gmail.TokenURL = oldAPI, oldToken })
	return f
}

func (f *apiFakeGmail) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sendRaws)
}

type gmailFixture struct {
	srv   *Server
	st    *store.Store
	key   []byte
	token string
	http  *httptest.Server
}

func newGmailFixture(t *testing.T) *gmailFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	srv := New(st, nil, syncmgr.NewManager(st))
	key := []byte("0123456789abcdef0123456789abcdef")
	token, err := auth.NewSession(st, "")
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	srv.keys[token] = key
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &gmailFixture{srv: srv, st: st, key: key, token: token, http: hs}
}

func (f *gmailFixture) doReq(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "namakani_session", Value: f.token})
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestGmailStartRequiresBaseURL(t *testing.T) {
	f := newGmailFixture(t)
	resp := f.doReq(t, http.MethodPost, "/api/accounts/gmail/start",
		`{"label":"我的 Gmail","email":"user@gmail.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("start without base_url status = %d; want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Base URL") {
		t.Fatalf("start without base_url error = %s; want it to name Base URL", body)
	}
}

func TestGmailOAuthStartAndCallbackCreatesPendingThenSaveCreatesAccount(t *testing.T) {
	fake := newAPIFakeGmail(t)
	f := newGmailFixture(t)
	cfg := f.st.AppSettings()
	cfg.BaseURL = "https://mail.example.com"
	if err := f.st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save base url: %v", err)
	}
	if err := f.srv.gmailOAuthSave("cid-1", "secret-1"); err != nil {
		t.Fatalf("save shared oauth: %v", err)
	}

	resp := f.doReq(t, http.MethodPost, "/api/accounts/gmail/start",
		`{"label":"我的 Gmail","email":"user@gmail.com"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d; want 200", resp.StatusCode)
	}
	var startOut struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&startOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, err := url.Parse(startOut.AuthURL)
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	q := authURL.Query()
	if q.Get("client_id") != "cid-1" || q.Get("access_type") != "offline" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("auth url params = %v; want client_id, offline access and a PKCE challenge", q)
	}
	state := q.Get("state")
	if state == "" {
		t.Fatal("auth url carries no state")
	}
	if q.Get("redirect_uri") != "https://mail.example.com/api/gmail/callback" {
		t.Fatalf("redirect_uri = %q; want it derived from base_url", q.Get("redirect_uri"))
	}

	resp = f.doReq(t, http.MethodGet, "/api/gmail/callback?code=good-code&state=wrong-state", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with wrong state = %d; want 400", resp.StatusCode)
	}
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("accounts after bad callback = %d, %v; want none", n, err)
	}

	resp = f.doReq(t, http.MethodGet, "/api/gmail/callback?code=good-code&state="+url.QueryEscape(state), "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d; want 302 after a successful exchange", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	locURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("callback redirect: %v", err)
	}
	pendingID := locURL.Query().Get("gmail_pending")
	if pendingID == "" || locURL.Fragment != "/settings" {
		t.Fatalf("callback redirect = %q; want a gmail_pending id on the settings route", loc)
	}
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("accounts after callback = %d, %v; want none before save", n, err)
	}

	resp = f.doReq(t, http.MethodGet, "/api/accounts/gmail/pending/"+pendingID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pending status = %d; want 200", resp.StatusCode)
	}
	var pending struct {
		Label        string   `json:"label"`
		Email        string   `json:"email"`
		Folders      int      `json:"folders"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if pending.Label != "我的 Gmail" || pending.Email != "user@gmail.com" || pending.Folders != 3 ||
		len(pending.Capabilities) != 1 || pending.Capabilities[0] != "GMAIL-API" {
		t.Fatalf("pending = %+v; want the submitted label, Google email, 3 folders and GMAIL-API", pending)
	}
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("accounts after reading pending = %d, %v; want still none", n, err)
	}

	resp = f.doReq(t, http.MethodPost, "/api/accounts", `{"kind":"gmail","gmail_pending_id":"`+pendingID+`"}`)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("save pending status = %d, body %s; want 201", resp.StatusCode, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.ID != 1 {
		t.Fatalf("created account id = %d; want 1", created.ID)
	}
	resp = f.doReq(t, http.MethodGet, "/api/accounts/gmail/pending/"+pendingID, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pending after save status = %d; want 404 because save consumes it", resp.StatusCode)
	}

	var kind, email string
	if err := f.st.DB.QueryRow("SELECT kind, email FROM accounts").Scan(&kind, &email); err != nil {
		t.Fatalf("account row: %v", err)
	}
	if kind != "gmail" || email != "user@gmail.com" {
		t.Fatalf("account = %s/%s; want gmail/user@gmail.com from the Google profile", kind, email)
	}
	acct, err := f.srv.loadMailAccount(1, f.key)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	creds, err := gmail.ParseCreds(acct.Password)
	if err != nil {
		t.Fatalf("stored creds: %v", err)
	}
	if creds.ClientID != "cid-1" || creds.ClientSecret != "secret-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("merged creds = %+v; want the shared client pair and the exchanged refresh token", creds)
	}
	var enc string
	if err := f.st.DB.QueryRow("SELECT enc_password FROM accounts WHERE id = 1").Scan(&enc); err != nil {
		t.Fatal(err)
	}
	rawStored, err := auth.Decrypt(f.key, enc)
	if err != nil {
		t.Fatal(err)
	}
	var storedShape struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(rawStored), &storedShape); err != nil {
		t.Fatal(err)
	}
	if storedShape.ClientID != "" || storedShape.ClientSecret != "" || storedShape.RefreshToken != "rt-1" {
		t.Fatalf("account row stores %+v; want only the refresh token", storedShape)
	}

	resp = f.doReq(t, http.MethodGet, "/api/gmail/callback?code=good-code&state="+url.QueryEscape(state), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d; want 400 because the state is single-use", resp.StatusCode)
	}
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 1 {
		t.Fatalf("accounts after replay = %d, %v; want still 1", n, err)
	}
	_ = fake
}

func createGmailPending(t *testing.T, f *gmailFixture) string {
	t.Helper()
	cfg := f.st.AppSettings()
	cfg.BaseURL = "https://mail.example.com"
	if err := f.st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save base url: %v", err)
	}
	if err := f.srv.gmailOAuthSave("cid-1", "secret-1"); err != nil {
		t.Fatalf("save shared oauth: %v", err)
	}
	resp := f.doReq(t, http.MethodPost, "/api/accounts/gmail/start",
		`{"label":"我的 Gmail","email":"user@gmail.com","from_name":"User","signature":"sig"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d; want 200", resp.StatusCode)
	}
	var startOut struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&startOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, err := url.Parse(startOut.AuthURL)
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	resp = f.doReq(t, http.MethodGet, "/api/gmail/callback?code=good-code&state="+url.QueryEscape(authURL.Query().Get("state")), "")
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status = %d; want 302", resp.StatusCode)
	}
	locURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("callback redirect: %v", err)
	}
	pendingID := locURL.Query().Get("gmail_pending")
	if pendingID == "" {
		t.Fatalf("callback redirect = %q; want a gmail_pending id", loc)
	}
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("accounts after callback = %d, %v; want none", n, err)
	}
	return pendingID
}

func TestGmailSaveWithoutPendingRequiresTest(t *testing.T) {
	f := newGmailFixture(t)
	resp := f.doReq(t, http.MethodPost, "/api/accounts", `{"kind":"gmail"}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "请先点测试连接") {
		t.Fatalf("save without pending = %d, %s; want 400 telling the user to test first", resp.StatusCode, body)
	}
}

func TestGmailPendingCancelDiscards(t *testing.T) {
	_ = newAPIFakeGmail(t)
	f := newGmailFixture(t)
	pendingID := createGmailPending(t, f)

	resp := f.doReq(t, http.MethodDelete, "/api/accounts/gmail/pending/"+pendingID, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete pending status = %d; want 200", resp.StatusCode)
	}
	resp = f.doReq(t, http.MethodGet, "/api/accounts/gmail/pending/"+pendingID, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pending after cancel status = %d; want 404", resp.StatusCode)
	}
	resp = f.doReq(t, http.MethodPost, "/api/accounts", `{"kind":"gmail","gmail_pending_id":"`+pendingID+`"}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "请先点测试连接") {
		t.Fatalf("save after cancel = %d, %s; want 400 telling the user to test first", resp.StatusCode, body)
	}
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("accounts after cancel = %d, %v; want none", n, err)
	}
}

func TestGmailAccountFormTestUsesStoredAccount(t *testing.T) {
	_ = newAPIFakeGmail(t)
	f := newGmailFixture(t)
	f.insertGmailAccount(t)

	resp := f.doReq(t, http.MethodPost, "/api/accounts/test", `{"id":1,"kind":"gmail","label":"g","email":"user@gmail.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("gmail form test status = %d, body %s; want 200", resp.StatusCode, body)
	}
	var out struct {
		OK           bool     `json:"ok"`
		Folders      int      `json:"folders"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Folders != 3 || len(out.Capabilities) != 1 || out.Capabilities[0] != "GMAIL-API" {
		t.Fatalf("gmail form test = %+v; want ok with 3 folders and GMAIL-API", out)
	}
}

func TestGmailAccountFormTestInvalidStoredToken(t *testing.T) {
	_ = newAPIFakeGmail(t)
	f := newGmailFixture(t)
	f.insertGmailAccount(t)
	enc, err := auth.Encrypt(f.key, gmail.Creds{ClientID: "cid-1", ClientSecret: "secret-1", RefreshToken: "bad-rt"}.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`UPDATE accounts SET enc_password = ? WHERE id = 1`, enc); err != nil {
		t.Fatal(err)
	}

	resp := f.doReq(t, http.MethodPost, "/api/accounts/test", `{"id":1,"kind":"gmail","label":"g","email":"user@gmail.com"}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "连接失败，请检查服务器设置和账号密码") {
		t.Fatalf("gmail form test with a bad token = %d, %s; want the 502 connection-failure form", resp.StatusCode, body)
	}
}

func TestGmailAccountFormTestWithoutStoredAccountRequiresAuthorization(t *testing.T) {
	f := newGmailFixture(t)
	resp := f.doReq(t, http.MethodPost, "/api/accounts/test", `{"kind":"gmail","label":"g","email":"user@gmail.com"}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "请先完成授权") {
		t.Fatalf("gmail form test without an account = %d, %s; want 400 telling the user to authorize first", resp.StatusCode, body)
	}
}

func TestGmailReauthCallbackStoresResultForDialogRestore(t *testing.T) {
	_ = newAPIFakeGmail(t)
	f := newGmailFixture(t)
	cfg := f.st.AppSettings()
	cfg.BaseURL = "https://mail.example.com"
	if err := f.st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save base url: %v", err)
	}
	if err := f.srv.gmailOAuthSave("cid-1", "secret-1"); err != nil {
		t.Fatalf("save shared oauth: %v", err)
	}
	f.insertGmailAccount(t)
	if _, err := f.st.DB.Exec(`UPDATE accounts SET from_name = 'User', signature = 'sig' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	stale, err := auth.Encrypt(f.key, gmail.TokenCredsJSON("rt-stale"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`UPDATE accounts SET enc_password = ? WHERE id = 1`, stale); err != nil {
		t.Fatal(err)
	}

	resp := f.doReq(t, http.MethodPost, "/api/accounts/gmail/start", `{"account_id":1}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reauth start status = %d; want 200", resp.StatusCode)
	}
	var startOut struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&startOut); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, err := url.Parse(startOut.AuthURL)
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatal("auth url carries no state")
	}

	resp = f.doReq(t, http.MethodGet, "/api/gmail/callback?code=good-code&state="+url.QueryEscape(state), "")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("reauth callback status = %d; want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	locURL, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("callback redirect: %v", err)
	}
	reauthID := locURL.Query().Get("gmail_reauthed")
	if reauthID == "" || locURL.Fragment != "/settings" {
		t.Fatalf("reauth callback redirect = %q; want a gmail_reauthed id on the settings route", loc)
	}
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n != 1 {
		t.Fatalf("accounts after reauth callback = %d, %v; want still 1", n, err)
	}

	acct, err := f.srv.loadMailAccount(1, f.key)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	creds, err := gmail.ParseCreds(acct.Password)
	if err != nil {
		t.Fatalf("stored creds: %v", err)
	}
	if creds.RefreshToken != "rt-1" {
		t.Fatalf("refresh token after reauth = %q; want rt-1 from the new grant", creds.RefreshToken)
	}

	resp = f.doReq(t, http.MethodGet, "/api/accounts/gmail/reauth/"+reauthID, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get reauth status = %d; want 200", resp.StatusCode)
	}
	var got struct {
		AccountID    int64    `json:"account_id"`
		Label        string   `json:"label"`
		Email        string   `json:"email"`
		FromName     string   `json:"from_name"`
		Signature    string   `json:"signature"`
		Folders      int      `json:"folders"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.AccountID != 1 || got.Label != "g" || got.Email != "user@gmail.com" ||
		got.FromName != "User" || got.Signature != "sig" || got.Folders != 3 ||
		len(got.Capabilities) != 1 || got.Capabilities[0] != "GMAIL-API" {
		t.Fatalf("reauth entry = %+v; want the account form fields, 3 folders and GMAIL-API", got)
	}
}

func TestGmailStartRequiresSharedOAuth(t *testing.T) {
	f := newGmailFixture(t)
	cfg := f.st.AppSettings()
	cfg.BaseURL = "https://mail.example.com"
	if err := f.st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save base url: %v", err)
	}
	resp := f.doReq(t, http.MethodPost, "/api/accounts/gmail/start",
		`{"label":"我的 Gmail","email":"user@gmail.com"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("start without shared oauth status = %d; want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "账号与认证") {
		t.Fatalf("start without shared oauth error = %s; want it to name the 账号与认证 settings", body)
	}
}

func TestGmailSharedOAuthSettingsAndMigration(t *testing.T) {
	f := newGmailFixture(t)
	f.insertGmailAccount(t)

	getShared := func() (string, bool) {
		resp := f.doReq(t, http.MethodGet, "/api/settings/gmail-oauth", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get shared oauth status = %d; want 200", resp.StatusCode)
		}
		var out struct {
			ClientID  string `json:"client_id"`
			HasSecret bool   `json:"has_secret"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.ClientID, out.HasSecret
	}
	if cid, has := getShared(); cid != "" || has {
		t.Fatalf("shared oauth before migration = %q/%v; want empty", cid, has)
	}

	acct, err := f.srv.loadMailAccount(1, f.key)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	creds, err := gmail.ParseCreds(acct.Password)
	if err != nil {
		t.Fatalf("merged creds: %v", err)
	}
	if creds.ClientID != "cid-1" || creds.ClientSecret != "secret-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("merged creds = %+v; want the legacy client pair plus rt-1", creds)
	}
	if cid, has := getShared(); cid != "cid-1" || !has {
		t.Fatalf("shared oauth after migration = %q/%v; want cid-1 with a secret", cid, has)
	}
	var enc string
	if err := f.st.DB.QueryRow("SELECT enc_password FROM accounts WHERE id = 1").Scan(&enc); err != nil {
		t.Fatal(err)
	}
	rawStored, err := auth.Decrypt(f.key, enc)
	if err != nil {
		t.Fatal(err)
	}
	var storedShape struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(rawStored), &storedShape); err != nil {
		t.Fatal(err)
	}
	if storedShape.ClientID != "" || storedShape.ClientSecret != "" || storedShape.RefreshToken != "rt-1" {
		t.Fatalf("account row after migration stores %+v; want only the refresh token", storedShape)
	}

	acct, err = f.srv.loadMailAccount(1, f.key)
	if err != nil {
		t.Fatalf("load account after migration: %v", err)
	}
	creds, err = gmail.ParseCreds(acct.Password)
	if err != nil {
		t.Fatalf("creds after migration: %v", err)
	}
	if creds.ClientID != "cid-1" || creds.ClientSecret != "secret-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("creds after migration = %+v; want the shared pair merged back in", creds)
	}

	resp := f.doReq(t, http.MethodPut, "/api/settings/gmail-oauth",
		`{"client_id":"cid-2","client_secret":""}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put shared oauth status = %d; want 200", resp.StatusCode)
	}
	if cid, has := getShared(); cid != "cid-2" || !has {
		t.Fatalf("shared oauth after put = %q/%v; want cid-2 with the secret kept", cid, has)
	}
	cfg := f.srv.gmailOAuthLoad()
	if cfg.ClientSecret != "secret-1" {
		t.Fatal("an empty secret in the put wiped the saved secret")
	}
}

func (f *gmailFixture) insertGmailAccount(t *testing.T) {
	t.Helper()
	creds := gmail.Creds{ClientID: "cid-1", ClientSecret: "secret-1", RefreshToken: "rt-1"}
	enc, err := auth.Encrypt(f.key, creds.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO accounts(id, label, email, kind, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'g', 'user@gmail.com', 'gmail', '', 0, 'none', '', 0, 'none', 'user@gmail.com', ?, 0)`, enc); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO folders(id, account_id, name, special_use) VALUES
		(1, 1, 'INBOX', 'inbox'), (2, 1, 'Sent', 'sent')`); err != nil {
		t.Fatal(err)
	}
}

func (f *gmailFixture) enqueueGmailSend(t *testing.T) int64 {
	t.Helper()
	opID, err := f.srv.enqueueSendOp(f.key, composeReq{
		AccountID: 1, To: "b@x.test", Subject: "hi", Text: "body",
	}, 9999999999, "")
	if err != nil {
		t.Fatalf("enqueue send: %v", err)
	}
	return opID
}

func TestGmailSendUndoBeforeDeliveryNeverCallsGoogle(t *testing.T) {
	fake := newAPIFakeGmail(t)
	f := newGmailFixture(t)
	f.insertGmailAccount(t)
	opID := f.enqueueGmailSend(t)

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/ops/%d/undo", opID), nil)
	req.SetPathValue("id", fmt.Sprint(opID))
	rec := httptest.NewRecorder()
	f.srv.handleUndoOp(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("undo status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := f.st.DB.Exec("UPDATE ops SET not_before = 0, next_attempt = 0 WHERE id = ?", opID); err != nil {
		t.Fatal(err)
	}
	f.srv.drainAccount(f.key, 1)
	if n := fake.sendCount(); n != 0 {
		t.Fatalf("google send called %d times for an undone send; want 0", n)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != store.OpCancelled {
		t.Fatalf("op status = %q; want cancelled", o.Status)
	}
}

func TestGmailSendDeliversAndFilesSentCopy(t *testing.T) {
	fake := newAPIFakeGmail(t)
	f := newGmailFixture(t)
	f.insertGmailAccount(t)
	opID := f.enqueueGmailSend(t)
	if _, err := f.st.DB.Exec("UPDATE ops SET not_before = 0, next_attempt = 0 WHERE id = ?", opID); err != nil {
		t.Fatal(err)
	}
	f.srv.drainAccount(f.key, 1)
	if n := fake.sendCount(); n != 1 {
		t.Fatalf("google send called %d times; want exactly 1", n)
	}
	raw, err := base64.RawURLEncoding.DecodeString(fake.sendRaws[0])
	if err != nil || !strings.Contains(string(raw), "Subject: hi") {
		t.Fatalf("sent raw = %q, %v; want the composed mime", raw, err)
	}
	o, err := f.st.GetOp(opID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != store.OpDone {
		t.Fatalf("op status = %q; want done (last error %q)", o.Status, o.LastError)
	}
	var rowID int64
	if err := f.st.DB.QueryRow("SELECT id FROM messages WHERE folder_id = 2").Scan(&rowID); err != nil {
		t.Fatalf("sent copy row: %v", err)
	}
	identity, err := f.st.IdentityForRow(1, rowID)
	if err != nil {
		t.Fatal(err)
	}
	if identity != "g:sent-1" {
		t.Fatalf("sent copy identity = %q; want g:sent-1", identity)
	}
}
