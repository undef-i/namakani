package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"namakani/internal/auth"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
)

// fakeIMAP is a scripted single-mailbox IMAP server holding exactly one
// message, just complete enough for one real sync round of the sync engine.
type fakeIMAP struct {
	ln  net.Listener
	raw string
}

const fakeIMAPRaw = "From: Alice <a@x.test>\r\n" +
	"To: Bob <b@x.test>\r\n" +
	"Subject: Hello token\r\n" +
	"Message-Id: <m1@x.test>\r\n" +
	"Date: Mon, 26 Oct 2026 10:00:00 +0800\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"hello body\r\n"

const fakeIMAPEnvelope = `("26-Oct-2026 10:00:00 +0800" "Hello token" (("Alice" NIL "a" "x.test")) (("Alice" NIL "a" "x.test")) (("Alice" NIL "a" "x.test")) (("Bob" NIL "b" "x.test")) NIL NIL NIL "<m1@x.test>")`

func startFakeIMAP(t *testing.T) *fakeIMAP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake imap listen: %v", err)
	}
	f := &fakeIMAP{ln: ln, raw: fakeIMAPRaw}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeIMAP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeIMAP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(s string) {
		_, _ = w.WriteString(s + "\r\n")
		_ = w.Flush()
	}
	send(`* OK [CAPABILITY IMAP4rev1] fake imap ready`)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag := fields[0]
		cmd := strings.ToUpper(fields[1])
		if cmd == "UID" && len(fields) >= 3 {
			cmd = "UID " + strings.ToUpper(fields[2])
		}
		switch cmd {
		case "CAPABILITY":
			send(`* CAPABILITY IMAP4rev1`)
			send(tag + " OK capability completed")
		case "LOGIN":
			send(tag + " OK login completed")
		case "LIST":
			send(`* LIST (\HasNoChildren) "/" INBOX`)
			send(tag + " OK list completed")
		case "SELECT", "EXAMINE":
			send(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`)
			send(`* 1 EXISTS`)
			send(`* 0 RECENT`)
			send(`* OK [UIDVALIDITY 777] uidvalidity`)
			send(`* OK [UIDNEXT 2] uidnext`)
			if cmd == "EXAMINE" {
				send(tag + " OK [READ-ONLY] examine completed")
			} else {
				send(tag + " OK [READ-WRITE] select completed")
			}
		case "UID SEARCH", "SEARCH":
			send(`* SEARCH 1`)
			send(tag + " OK search completed")
		case "FETCH", "UID FETCH":
			f.fetch(w, line)
			send(tag + " OK fetch completed")
		case "LOGOUT":
			send(`* BYE bye`)
			send(tag + " OK logout completed")
			return
		default:
			send(tag + " OK completed")
		}
	}
}

// fetch answers one FETCH/UID FETCH for the single stored message. Only the
// items actually requested on the command line are emitted.
func (f *fakeIMAP) fetch(w *bufio.Writer, line string) {
	upper := strings.ToUpper(line)
	var b strings.Builder
	b.WriteString("* 1 FETCH (UID 1")
	if strings.Contains(upper, "FLAGS") {
		b.WriteString(" FLAGS ()")
	}
	if strings.Contains(upper, "INTERNALDATE") {
		b.WriteString(` INTERNALDATE "26-Oct-2026 10:00:00 +0800"`)
	}
	if strings.Contains(upper, "RFC822.SIZE") {
		fmt.Fprintf(&b, " RFC822.SIZE %d", len(f.raw))
	}
	if strings.Contains(upper, "ENVELOPE") {
		b.WriteString(" ENVELOPE " + fakeIMAPEnvelope)
	}
	if strings.Contains(upper, "BODYSTRUCTURE") {
		b.WriteString(` BODYSTRUCTURE ("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "7BIT" ` +
			strconv.Itoa(len(f.raw)) + ` 8 NIL NIL NIL)`)
	}
	// Body sections: echo each requested BODY.PEEK[...] back as BODY[...]
	// with the matching literal payload.
	rest := line
	for {
		i := strings.Index(strings.ToUpper(rest), "BODY")
		if i < 0 {
			break
		}
		rest = rest[i:]
		open := strings.Index(rest, "[")
		if open < 0 {
			break
		}
		end := strings.Index(rest[open:], "]")
		if end < 0 {
			break
		}
		spec := rest[open+1 : open+end]
		rest = rest[open+end+1:]
		var payload string
		uspec := strings.ToUpper(spec)
		switch {
		case spec == "":
			payload = f.raw
		case strings.HasPrefix(uspec, "HEADER.FIELDS") && strings.Contains(uspec, "MESSAGE-ID"):
			payload = "Message-Id: <m1@x.test>\r\n"
		case strings.HasPrefix(uspec, "HEADER.FIELDS"):
			payload = ""
		default:
			payload = f.raw
		}
		fmt.Fprintf(&b, " BODY[%s] {%d}\r\n%s", spec, len(payload), payload)
	}
	b.WriteString(")\r\n")
	_, _ = w.WriteString(b.String())
	_ = w.Flush()
}

// tokenFixture wires a store with a set-up master password and a first
// server whose operator logs in through the real handler. The mail account
// is inserted only at wake time so the first server never syncs it.
type tokenFixture struct {
	st       *store.Store
	srv      *Server
	imapPort int
	key      []byte
	cookie   *http.Cookie
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	imapSrv := startFakeIMAP(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "namakani.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	if err := auth.SetupMaster(st, "test-master-123"); err != nil {
		t.Fatalf("setup master: %v", err)
	}
	key, ok, err := auth.VerifyMaster(st, "test-master-123")
	if err != nil || !ok {
		t.Fatalf("verify master: ok=%v err=%v", ok, err)
	}
	srv := New(st, nil, syncmgr.NewManager(st))
	f := &tokenFixture{st: st, srv: srv, imapPort: imapSrv.port(), key: key}
	rec := f.do(srv, http.MethodPost, "/api/login", `{"password":"test-master-123"}`, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "namakani_session" {
			f.cookie = c
		}
	}
	if f.cookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	return f
}

func (f *tokenFixture) do(srv *Server, method, path, body string, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *tokenFixture) createToken(t *testing.T, srv *Server, label string) (int64, string) {
	t.Helper()
	rec := f.do(srv, http.MethodPost, "/api/settings/access-tokens", `{"label":`+strconv.Quote(label)+`}`, f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("create token: status %d body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if out.ID == 0 || out.Token == "" {
		t.Fatalf("create response missing id or plaintext token: %s", rec.Body.String())
	}
	return out.ID, out.Token
}

func (f *tokenFixture) insertAccount(t *testing.T) {
	t.Helper()
	enc, err := auth.Encrypt(f.key, "pw123")
	if err != nil {
		t.Fatalf("encrypt account password: %v", err)
	}
	if _, err := f.st.DB.Exec(`INSERT INTO accounts(id, label, email, imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls, username, enc_password, created_at)
		VALUES(1, 'dev', 'b@x.test', '127.0.0.1', ?, 'none', '127.0.0.1', 25, 'none', 'b@x.test', ?, 0)`, f.imapPort, enc); err != nil {
		t.Fatalf("insert account: %v", err)
	}
}

// restart builds a fresh server (empty in-memory key table, fresh sync
// manager) over the same store, standing in for a process restart.
func (f *tokenFixture) restart() *Server {
	return New(f.st, nil, syncmgr.NewManager(f.st))
}

func waitSyncIdle(t *testing.T, srv *Server, accountID int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := srv.mgr.Status(accountID)
		if st.State == "idle" && st.LastSyncAt > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := srv.mgr.Status(accountID)
	t.Fatalf("sync worker for account %d never completed a round: state=%q last_error=%q", accountID, st.State, st.LastError)
}

func TestAccessTokenColdRecoveryWakesSync(t *testing.T) {
	f := newTokenFixture(t)
	_, token := f.createToken(t, f.srv, "dev cli")
	f.insertAccount(t)

	srv2 := f.restart()
	if got := srv2.mgr.Status(1).State; got != "stopped" {
		t.Fatalf("fresh server must not have a worker running before any request, got %q", got)
	}
	// Zero cookies, bearer only: must authenticate fully and cold-recover K.
	rec := f.do(srv2, http.MethodGet, "/api/accounts", "", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer request after restart: status %d body %s", rec.Code, rec.Body.String())
	}
	waitSyncIdle(t, srv2, 1)
	var n int
	var mid string
	if err := f.st.DB.QueryRow("SELECT COUNT(*), COALESCE(MAX(message_id), '') FROM messages WHERE account_id = 1").Scan(&n, &mid); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 1 || mid != "m1@x.test" {
		t.Fatalf("one sync round must land the mailbox message: count=%d message_id=%q", n, mid)
	}
}

func TestAccessTokenRevocationIsImmediate(t *testing.T) {
	f := newTokenFixture(t)
	id, token := f.createToken(t, f.srv, "dev cli")
	f.insertAccount(t)

	srv2 := f.restart()
	if rec := f.do(srv2, http.MethodGet, "/api/accounts", "", nil, token); rec.Code != http.StatusOK {
		t.Fatalf("bearer request before revoke: status %d", rec.Code)
	}
	rec := f.do(srv2, http.MethodDelete, fmt.Sprintf("/api/settings/access-tokens/%d", id), "", f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(srv2, http.MethodGet, "/api/accounts", "", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token must get 401 on the very next request, got %d", rec.Code)
	}
	srv3 := f.restart()
	if rec := f.do(srv3, http.MethodGet, "/api/accounts", "", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token must stay 401 after a restart, got %d", rec.Code)
	}
	if len(srv3.keys) != 0 {
		t.Fatal("a revoked token must not recover any key after a restart")
	}
}

func TestAccessTokenRejectsBadAndMissingCredentials(t *testing.T) {
	f := newTokenFixture(t)
	srv := f.restart()
	if rec := f.do(srv, http.MethodGet, "/api/accounts", "", nil, "0000000000000000000000000000000000000000000000000000000000000000"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown bearer token must get 401, got %d", rec.Code)
	}
	if rec := f.do(srv, http.MethodGet, "/api/accounts", "", nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("request without any credential must get 401, got %d", rec.Code)
	}
}

func TestAccessTokenManagementRequiresSession(t *testing.T) {
	f := newTokenFixture(t)
	_, token := f.createToken(t, f.srv, "dev cli")

	if rec := f.do(f.srv, http.MethodPost, "/api/settings/access-tokens", `{"label":"x"}`, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("create without a session must get 401, got %d", rec.Code)
	}
	if rec := f.do(f.srv, http.MethodPost, "/api/settings/access-tokens", `{"label":"x"}`, nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token must not create tokens, got %d", rec.Code)
	}
	if rec := f.do(f.srv, http.MethodGet, "/api/settings/access-tokens", "", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list with a token must get 401, got %d", rec.Code)
	}
	if rec := f.do(f.srv, http.MethodDelete, "/api/settings/access-tokens/1", "", nil, token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoke with a token must get 401, got %d", rec.Code)
	}
	rec := f.do(f.srv, http.MethodGet, "/api/settings/access-tokens", "", f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list with a session: status %d body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatal("the token list must never contain a plaintext token")
	}
	if !strings.Contains(rec.Body.String(), "dev cli") {
		t.Fatalf("list must show the token label: %s", rec.Body.String())
	}
}
