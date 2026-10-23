package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	netmail "net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"namakani/internal/auth"
	"namakani/internal/mail"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"

	"github.com/emersion/go-imap/v2"
)

type Server struct {
	st            *store.Store
	uiFS          fs.FS
	mgr           *syncmgr.Manager
	hub           *hub
	serverKey     []byte
	mu            sync.Mutex
	keys          map[string][]byte
	loginMu       sync.Mutex
	loginFails    int
	loginLastFail time.Time
	tgSeenMu      sync.Mutex
	tgSeen        map[string]tgChat
}

func New(st *store.Store, uiFS fs.FS, mgr *syncmgr.Manager) *Server {
	s := &Server{st: st, uiFS: uiFS, mgr: mgr, hub: newHub(), keys: make(map[string][]byte), tgSeen: make(map[string]tgChat)}
	go s.tgSupervise()
	go s.scheduleLoop()
	if key, err := auth.LoadOrCreateServerKey(filepath.Join(st.DataDir(), "server.key")); err == nil {
		s.serverKey = key
	} else {
		log.Printf("namakani: server key unavailable, sessions will not survive restarts: %v", err)
	}
	mgr.OnChange = s.onSyncChange
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/setup", s.handleSetup)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.authOnly(s.handleLogout))
	mux.HandleFunc("GET /api/accounts", s.authOnly(s.handleListAccounts))
	mux.HandleFunc("POST /api/accounts", s.authOnly(s.handleCreateAccount))
	mux.HandleFunc("PUT /api/accounts/{id}", s.authOnly(s.handleUpdateAccount))
	mux.HandleFunc("DELETE /api/accounts/{id}", s.authOnly(s.handleDeleteAccount))
	mux.HandleFunc("POST /api/accounts/{id}/test", s.authOnly(s.handleTestAccount))
	mux.HandleFunc("GET /api/accounts/{id}/sync", s.authOnly(s.handleSyncStatus))
	mux.HandleFunc("GET /api/folders", s.authOnly(s.handleListFolders))
	mux.HandleFunc("GET /api/messages", s.authOnly(s.handleListMessages))
	mux.HandleFunc("GET /api/messages/{id}", s.authOnly(s.handleGetMessage))
	mux.HandleFunc("GET /api/messages/{id}/attachments/{attId}", s.authOnly(s.handleDownloadAttachment))
	mux.HandleFunc("POST /api/messages/{id}/flags", s.authOnly(s.handleSetFlags))
	mux.HandleFunc("POST /api/messages/{id}/move", s.authOnly(s.handleMoveMessage))
	mux.HandleFunc("POST /api/messages/{id}/delete", s.authOnly(s.handleDeleteMessage))
	mux.HandleFunc("POST /api/send", s.authOnly(s.handleSend))
	mux.HandleFunc("POST /api/send/schedule", s.authOnly(s.handleScheduleSend))
	mux.HandleFunc("GET /api/scheduled", s.authOnly(s.handleListScheduled))
	mux.HandleFunc("DELETE /api/scheduled/{id}", s.authOnly(s.handleCancelScheduled))
	mux.HandleFunc("GET /api/contacts", s.authOnly(s.handleContacts))
	mux.HandleFunc("POST /api/drafts", s.authOnly(s.handleSaveDraft))
	mux.HandleFunc("GET /api/search", s.authOnly(s.handleSearch))
	mux.HandleFunc("GET /api/events", s.authOnly(s.handleEvents))
	mux.HandleFunc("GET /api/settings/app", s.authOnly(s.handleGetAppSettings))
	mux.HandleFunc("PUT /api/settings/app", s.authOnly(s.handlePutAppSettings))
	mux.HandleFunc("GET /api/settings/telegram", s.authOnly(s.handleGetTelegram))
	mux.HandleFunc("PUT /api/settings/telegram", s.authOnly(s.handlePutTelegram))
	mux.HandleFunc("POST /api/settings/telegram/discover", s.authOnly(s.handleTelegramDiscover))
	mux.HandleFunc("POST /api/settings/telegram/test", s.authOnly(s.handleTelegramTest))
	mux.HandleFunc("GET /api/settings/webpush", s.authOnly(s.handleGetWebPush))
	mux.HandleFunc("POST /api/settings/webpush/test", s.authOnly(s.handleWebPushTest))
	mux.HandleFunc("POST /api/push/subscribe", s.authOnly(s.handlePushSubscribe))
	mux.HandleFunc("POST /api/push/unsubscribe", s.authOnly(s.handlePushUnsubscribe))
	mux.HandleFunc("POST /api/messages/mark-all-read", s.authOnly(s.handleMarkAllRead))
	mux.Handle("/", s.spaHandler())
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie("wm_session")
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) credentialKey(r *http.Request) ([]byte, bool) {
	token := s.sessionToken(r)
	if token == "" {
		return nil, false
	}
	s.mu.Lock()
	key, ok := s.keys[token]
	s.mu.Unlock()
	if ok {
		return key, true
	}
	if s.serverKey == nil {
		return nil, false
	}
	wrapped, expiresAt, exists, err := auth.SessionInfo(s.st, token)
	if err != nil || !exists || wrapped == "" {
		return nil, false
	}
	if time.Now().Unix() > expiresAt {
		return nil, false
	}
	plain, err := auth.Decrypt(s.serverKey, wrapped)
	if err != nil {
		return nil, false
	}
	key = []byte(plain)
	s.mu.Lock()
	s.keys[token] = key
	s.mu.Unlock()
	s.startAllWorkers(key)
	return key, true
}

func (s *Server) authOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.credentialKey(r); !ok {
			writeErr(w, http.StatusUnauthorized, "not logged in")
			return
		}
		s.renewSession(w, r)
		next(w, r)
	}
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "wm_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL / time.Second),
	})
}

func (s *Server) renewSession(w http.ResponseWriter, r *http.Request) {
	token := s.sessionToken(r)
	if token == "" {
		return
	}
	s.setSessionCookie(w, r, token)
	_ = auth.TouchSession(s.st, token)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, key []byte) error {
	wrapped := ""
	if s.serverKey != nil {
		sealed, err := auth.Encrypt(s.serverKey, string(key))
		if err != nil {
			return err
		}
		wrapped = sealed
	}
	token, err := auth.NewSession(s.st, wrapped)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys[token] = key
	s.mu.Unlock()
	s.setSessionCookie(w, r, token)
	return nil
}

var BuildVersion = "dev"

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	setup, err := auth.IsSetup(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "state error")
		return
	}
	_, authed := s.credentialKey(r)
	if authed {
		s.renewSession(w, r)
	}
	out := map[string]any{"setup": setup, "authed": authed, "version": BuildVersion}
	if authed {
		out["app_settings"] = s.st.AppSettings()
	}
	writeJSON(w, http.StatusOK, out)
}

type passwordReq struct {
	Password string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	setup, err := auth.IsSetup(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "state error")
		return
	}
	if setup {
		writeErr(w, http.StatusConflict, "already set up")
		return
	}
	var req passwordReq
	if err := decodeJSON(r, &req); err != nil || len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if err := auth.SetupMaster(s.st, req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, "setup failed")
		return
	}
	key, ok, err := auth.VerifyMaster(s.st, req.Password)
	if err != nil || !ok {
		writeErr(w, http.StatusInternalServerError, "setup verify failed")
		return
	}
	if err := s.startSession(w, r, key); err != nil {
		writeErr(w, http.StatusInternalServerError, "session failed")
		return
	}
	s.startAllWorkers(key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) loginWait() time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails < 5 {
		return 0
	}
	exp := s.loginFails - 5
	if exp > 8 {
		exp = 8
	}
	if w := time.Until(s.loginLastFail.Add(time.Duration(1<<exp) * time.Second)); w > 0 {
		return w
	}
	return 0
}

func (s *Server) noteLoginFailure() {
	s.loginMu.Lock()
	s.loginFails++
	s.loginLastFail = time.Now()
	s.loginMu.Unlock()
}

func (s *Server) resetLoginFailures() {
	s.loginMu.Lock()
	s.loginFails = 0
	s.loginMu.Unlock()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "password required")
		return
	}
	if s.loginWait() > 0 {
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	key, ok, err := auth.VerifyMaster(s.st, req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "login failed")
		return
	}
	if !ok {
		s.noteLoginFailure()
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.resetLoginFailures()
	if err := s.startSession(w, r, key); err != nil {
		writeErr(w, http.StatusInternalServerError, "session failed")
		return
	}
	s.startAllWorkers(key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := s.sessionToken(r)
	s.mgr.StopAll()
	_ = auth.DeleteSession(s.st, token)
	s.mu.Lock()
	delete(s.keys, token)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "wm_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type accountIn struct {
	Label         string `json:"label"`
	Email         string `json:"email"`
	ImapHost      string `json:"imap_host"`
	ImapPort      any    `json:"imap_port"`
	ImapTLS       string `json:"imap_tls"`
	SmtpHost      string `json:"smtp_host"`
	SmtpPort      any    `json:"smtp_port"`
	SmtpTLS       string `json:"smtp_tls"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Signature     string `json:"signature"`
	FromName      string `json:"from_name"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
}

type accountOut struct {
	ID            int64  `json:"id"`
	Label         string `json:"label"`
	Email         string `json:"email"`
	ImapHost      string `json:"imap_host"`
	ImapPort      int    `json:"imap_port"`
	ImapTLS       string `json:"imap_tls"`
	SmtpHost      string `json:"smtp_host"`
	SmtpPort      int    `json:"smtp_port"`
	SmtpTLS       string `json:"smtp_tls"`
	Username      string `json:"username"`
	Signature     string `json:"signature"`
	FromName      string `json:"from_name"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
}

func validateAccount(in *accountIn, passwordRequired bool) (imapPort, smtpPort int, err error) {
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" ||
		strings.TrimSpace(in.ImapHost) == "" || strings.TrimSpace(in.SmtpHost) == "" ||
		strings.TrimSpace(in.Username) == "" {
		return 0, 0, errors.New("label, email, hosts and username are required")
	}
	if passwordRequired && in.Password == "" {
		return 0, 0, errors.New("password is required")
	}
	if !auth.ValidTLSMode(in.ImapTLS) || !auth.ValidTLSMode(in.SmtpTLS) {
		return 0, 0, errors.New("tls mode must be tls, starttls or none")
	}
	imapPort, err = auth.ParsePort(in.ImapPort)
	if err != nil {
		return 0, 0, err
	}
	smtpPort, err = auth.ParsePort(in.SmtpPort)
	if err != nil {
		return 0, 0, err
	}
	return imapPort, smtpPort, nil
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT id, label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, signature, from_name, tls_skip_verify FROM accounts ORDER BY sort, id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	defer rows.Close()
	out := []accountOut{}
	for rows.Next() {
		var a accountOut
		if err := rows.Scan(&a.ID, &a.Label, &a.Email, &a.ImapHost, &a.ImapPort, &a.ImapTLS,
			&a.SmtpHost, &a.SmtpPort, &a.SmtpTLS, &a.Username, &a.Signature, &a.FromName, &a.TLSSkipVerify); err != nil {
			writeErr(w, http.StatusInternalServerError, "list failed")
			return
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var in accountIn
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	imapPort, smtpPort, err := validateAccount(&in, true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enc, err := auth.Encrypt(key, in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypt failed")
		return
	}
	res, err := s.st.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, signature, from_name, tls_skip_verify, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
		in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, enc, in.Signature, in.FromName, in.TLSSkipVerify, time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "insert failed")
		return
	}
	id, _ := res.LastInsertId()
	s.startWorkerFor(id, key)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var in accountIn
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	imapPort, smtpPort, err := validateAccount(&in, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if in.Password != "" {
		enc, err := auth.Encrypt(key, in.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt failed")
			return
		}
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, imap_host=?, imap_port=?, imap_tls=?,
			smtp_host=?, smtp_port=?, smtp_tls=?, username=?, enc_password=?, signature=?, from_name=?, tls_skip_verify=? WHERE id=?`,
			in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
			in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, enc, in.Signature, in.FromName, in.TLSSkipVerify, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "update failed")
			return
		}
	} else {
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, imap_host=?, imap_port=?, imap_tls=?,
			smtp_host=?, smtp_port=?, smtp_tls=?, username=?, signature=?, from_name=?, tls_skip_verify=? WHERE id=?`,
			in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
			in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, in.Signature, in.FromName, in.TLSSkipVerify, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	if idNum, err := strconv.ParseInt(id, 10, 64); err == nil {
		s.mgr.Stop(idNum)
		s.startWorkerFor(idNum, key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if idNum, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		s.mgr.Stop(idNum)
	}
	_, err := s.st.DB.Exec("DELETE FROM accounts WHERE id = ?", r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) loadMailAccount(id int64, key []byte) (mail.Account, error) {
	var a mail.Account
	var enc string
	err := s.st.DB.QueryRow(`SELECT label, from_name, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, tls_skip_verify FROM accounts WHERE id = ?`, id).
		Scan(&a.Label, &a.FromName, &a.Email, &a.ImapHost, &a.ImapPort, &a.ImapTLS,
			&a.SMTPHost, &a.SMTPPort, &a.SMTPTLS, &a.Username, &enc, &a.TLSSkipVerify)
	if err != nil {
		return a, err
	}
	a.ID = id
	pw, err := auth.Decrypt(key, enc)
	if err != nil {
		return a, err
	}
	a.Password = pw
	return a, nil
}

func (s *Server) startWorkerFor(id int64, key []byte) {
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		return
	}
	s.mgr.Start(acct)
}

func (s *Server) startAllWorkers(key []byte) {
	rows, err := s.st.DB.Query("SELECT id FROM accounts")
	if err != nil {
		return
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.startWorkerFor(id, key)
	}
}

func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad account id")
		return
	}
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	cl, err := mail.Connect(acct, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cl.Close()
	caps := make([]string, 0, len(cl.Caps))
	for cap := range cl.Caps {
		caps = append(caps, string(cap))
	}
	sort.Strings(caps)
	folders, err := cl.ListFolders()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list folders: "+err.Error())
		return
	}
	type folderJSON struct {
		Name       string `json:"name"`
		SpecialUse string `json:"special_use"`
	}
	out := make([]folderJSON, 0, len(folders))
	for _, f := range folders {
		out = append(out, folderJSON{Name: f.Name, SpecialUse: f.SpecialUse})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capabilities": caps,
		"folders":      out,
	})
}

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad account id")
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Status(id))
}

type folderOut struct {
	ID           int64  `json:"id"`
	AccountID    int64  `json:"account_id"`
	AccountLabel string `json:"account_label"`
	Name         string `json:"name"`
	SpecialUse   string `json:"special_use"`
	Total        int    `json:"total"`
	Unread       int    `json:"unread"`
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT f.id, f.account_id, a.label, f.name, f.special_use, f.total, f.unread
		FROM folders f JOIN accounts a ON a.id = f.account_id
		ORDER BY f.account_id, f.id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list folders failed")
		return
	}
	defer rows.Close()
	out := []folderOut{}
	for rows.Next() {
		var f folderOut
		if err := rows.Scan(&f.ID, &f.AccountID, &f.AccountLabel, &f.Name, &f.SpecialUse, &f.Total, &f.Unread); err != nil {
			writeErr(w, http.StatusInternalServerError, "list folders failed")
			return
		}
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
}

type messageOut struct {
	ID            int64  `json:"id"`
	AccountID     int64  `json:"account_id"`
	AccountLabel  string `json:"account_label"`
	FolderID      int64  `json:"folder_id"`
	FolderName    string `json:"folder_name"`
	FolderSpecial string `json:"folder_special"`
	ThreadKey     string `json:"thread_key"`
	Subject       string `json:"subject"`
	FromName      string `json:"from_name"`
	FromEmail     string `json:"from_email"`
	DateTS        int64  `json:"date_ts"`
	Flags         uint32 `json:"flags"`
	HasAttach     bool   `json:"has_attach"`
	Snippet       string `json:"snippet"`
	Size          int64  `json:"size"`
}

type threadOut struct {
	messageOut
	ThreadCount  int `json:"thread_count"`
	ThreadUnread int `json:"thread_unread"`
}

const messageCols = `m.id, m.account_id, a.label, m.folder_id, f.name, f.special_use,
	m.thread_key, m.subject, m.from_json, m.date_ts, m.flags, m.has_attach, m.snippet, m.size`

const messageJoins = `FROM messages m
	JOIN accounts a ON a.id = m.account_id
	JOIN folders f ON f.id = m.folder_id`

func scanMessage(rows interface {
	Next() bool
	Scan(...any) error
}, mo *messageOut) error {
	var fromJSON string
	var hasAttach int
	if err := rows.Scan(&mo.ID, &mo.AccountID, &mo.AccountLabel, &mo.FolderID, &mo.FolderName,
		&mo.FolderSpecial, &mo.ThreadKey, &mo.Subject, &fromJSON, &mo.DateTS, &mo.Flags,
		&hasAttach, &mo.Snippet, &mo.Size); err != nil {
		return err
	}
	mo.HasAttach = hasAttach != 0
	var addrs []mail.Addr
	if err := json.Unmarshal([]byte(fromJSON), &addrs); err == nil && len(addrs) > 0 {
		mo.FromName = addrs[0].Name
		mo.FromEmail = addrs[0].Email
	}
	return nil
}

func (s *Server) messageRowsByIDs(ids []int64) []messageOut {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	dbRows, err := s.st.DB.Query(`SELECT `+messageCols+` `+messageJoins+
		` WHERE m.id IN (`+strings.Join(ph, ",")+`) ORDER BY m.date_ts DESC, m.id DESC`, args...)
	if err != nil {
		return nil
	}
	defer dbRows.Close()
	out := []messageOut{}
	for dbRows.Next() {
		var mo messageOut
		if err := scanMessage(dbRows, &mo); err == nil {
			out = append(out, mo)
		}
	}
	return out
}

func listScope(q map[string][]string) (string, []any) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	where := "1 = 1"
	var args []any
	if fid := get("folder_id"); fid != "" {
		where += " AND m.folder_id = ?"
		args = append(args, fid)
	} else {
		special := get("special")
		if special == "" {
			special = "inbox"
		}
		where += " AND f.special_use = ?"
		args = append(args, special)
	}
	if tk := get("thread_key"); tk != "" {
		where += " AND m.account_id = ? AND COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id) = ?"
		args = append(args, get("account_id"), tk)
	}
	return where, args
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 || limit > 500 {
		limit = 60
	}
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	where, args := listScope(q)

	if tk := q.Get("thread_key"); tk != "" {
		dbRows, err := s.st.DB.Query(`SELECT `+messageCols+` `+messageJoins+
			` WHERE `+where+` ORDER BY m.date_ts ASC, m.id ASC`, args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list messages failed")
			return
		}
		defer dbRows.Close()
		out := []messageOut{}
		for dbRows.Next() {
			var mo messageOut
			if err := scanMessage(dbRows, &mo); err != nil {
				writeErr(w, http.StatusInternalServerError, "list messages failed")
				return
			}
			out = append(out, mo)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	if q.Get("threaded") == "1" {
		keyExpr := `COALESCE(NULLIF(m.thread_key, ''), 'id:' || m.id)`
		query := `WITH agg AS (
				SELECT m.account_id AS acct, ` + keyExpr + ` AS tk,
					COUNT(*) AS cnt,
					SUM(CASE WHEN (m.flags & 1) = 0 THEN 1 ELSE 0 END) AS unread
				FROM messages m JOIN folders f ON f.id = m.folder_id
				WHERE ` + where + ` GROUP BY m.account_id, tk
			),
			ranked AS (
				SELECT m.id AS mid,
					ROW_NUMBER() OVER (PARTITION BY m.account_id, ` + keyExpr + `
						ORDER BY m.date_ts DESC, m.id DESC) AS rn
				FROM messages m JOIN folders f ON f.id = m.folder_id
				WHERE ` + where + `
			)
			SELECT ` + messageCols + `, agg.cnt, agg.unread
			` + messageJoins + `
			JOIN ranked ON ranked.mid = m.id AND ranked.rn = 1
			JOIN agg ON agg.acct = m.account_id AND agg.tk = ` + keyExpr + `
			ORDER BY m.date_ts DESC, m.id DESC LIMIT ? OFFSET ?`
		dbArgs := append(append(append([]any{}, args...), args...), limit, offset)
		dbRows, err := s.st.DB.Query(query, dbArgs...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list threads failed")
			return
		}
		defer dbRows.Close()
		out := []threadOut{}
		for dbRows.Next() {
			var to threadOut
			var fromJSON string
			var hasAttach int
			if err := dbRows.Scan(&to.ID, &to.AccountID, &to.AccountLabel, &to.FolderID, &to.FolderName,
				&to.FolderSpecial, &to.ThreadKey, &to.Subject, &fromJSON, &to.DateTS, &to.Flags,
				&hasAttach, &to.Snippet, &to.Size, &to.ThreadCount, &to.ThreadUnread); err != nil {
				writeErr(w, http.StatusInternalServerError, "list threads failed")
				return
			}
			to.HasAttach = hasAttach != 0
			var addrs []mail.Addr
			if err := json.Unmarshal([]byte(fromJSON), &addrs); err == nil && len(addrs) > 0 {
				to.FromName = addrs[0].Name
				to.FromEmail = addrs[0].Email
			}
			if to.ThreadKey == "" {
				to.ThreadKey = "id:" + strconv.FormatInt(to.ID, 10)
			}
			out = append(out, to)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	dbRows, err := s.st.DB.Query(`SELECT `+messageCols+` `+messageJoins+
		` WHERE `+where+` ORDER BY m.date_ts DESC, m.id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list messages failed")
		return
	}
	defer dbRows.Close()
	out := []messageOut{}
	for dbRows.Next() {
		var mo messageOut
		if err := scanMessage(dbRows, &mo); err != nil {
			writeErr(w, http.StatusInternalServerError, "list messages failed")
			return
		}
		out = append(out, mo)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) openClient(key []byte, accountID int64) (*mail.Client, error) {
	acct, err := s.loadMailAccount(accountID, key)
	if err != nil {
		return nil, err
	}
	return mail.Connect(acct, nil)
}

type messageDetail struct {
	ID           int64                 `json:"id"`
	AccountID    int64                 `json:"account_id"`
	AccountLabel string                `json:"account_label"`
	FolderID     int64                 `json:"folder_id"`
	FolderName   string                `json:"folder_name"`
	Subject      string                `json:"subject"`
	From         *mail.Addr            `json:"from"`
	To           []mail.Addr           `json:"to"`
	Cc           []mail.Addr           `json:"cc"`
	DateTS       int64                 `json:"date_ts"`
	Flags        uint32                `json:"flags"`
	Size         int64                 `json:"size"`
	HasAttach    bool                  `json:"has_attach"`
	Text         string                `json:"text"`
	HTML         string                `json:"html"`
	Attachments  []store.AttachmentRow `json:"attachments"`
}

func parseAddrs(jsonStr string) []mail.Addr {
	var addrs []mail.Addr
	if err := json.Unmarshal([]byte(jsonStr), &addrs); err != nil || addrs == nil {
		return []mail.Addr{}
	}
	return addrs
}

func (s *Server) ensureBody(key []byte, messageID int64, route store.MessageRoute) (string, string, []store.AttachmentRow, error) {
	if route.BodyState == 1 {
		text, html, _, err := s.st.GetBody(messageID)
		if err != nil {
			return "", "", nil, err
		}
		atts, err := s.st.ListAttachments(messageID)
		return text, html, atts, err
	}
	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		return "", "", nil, err
	}
	defer cl.Close()
	if _, err := cl.Select(route.Folder); err != nil {
		return "", "", nil, err
	}
	raw, err := cl.FetchRaw(route.UID)
	if err != nil {
		return "", "", nil, err
	}
	parsed, err := mail.ParseBody(raw)
	if err != nil {
		return "", "", nil, err
	}
	if err := s.st.SaveBody(messageID, parsed.Text, parsed.HTML, mail.MakeSnippet(parsed.Text, parsed.HTML)); err != nil {
		return "", "", nil, err
	}
	rows := make([]store.AttachmentRow, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		rows = append(rows, store.AttachmentRow{
			PartPath: a.PartPath, Filename: a.Filename, Mime: a.Mime,
			Size: a.Size, ContentID: a.ContentID, Inline: a.Inline,
		})
	}
	if len(parsed.References) > 0 || parsed.InReplyTo != "" {
		_ = s.st.RecomputeThreadKey(route.AccountID, messageID, parsed.References, parsed.InReplyTo)
	}
	atts, err := s.st.ReplaceAttachments(messageID, rows)
	if err != nil {
		return "", "", nil, err
	}
	return parsed.Text, parsed.HTML, atts, nil
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	var (
		d         messageDetail
		fromJSON  string
		toJSON    string
		ccJSON    string
		hasAttach int
	)
	err = s.st.DB.QueryRow(`SELECT m.account_id, a.label, m.folder_id, f.name, m.subject,
		m.from_json, m.to_json, m.cc_json, m.date_ts, m.flags, m.size, m.has_attach
		FROM messages m JOIN accounts a ON a.id = m.account_id JOIN folders f ON f.id = m.folder_id
		WHERE m.id = ?`, id).
		Scan(&d.AccountID, &d.AccountLabel, &d.FolderID, &d.FolderName, &d.Subject,
			&fromJSON, &toJSON, &ccJSON, &d.DateTS, &d.Flags, &d.Size, &hasAttach)
	if err != nil {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	d.ID = id
	d.HasAttach = hasAttach != 0
	from := parseAddrs(fromJSON)
	if len(from) > 0 {
		d.From = &from[0]
	}
	d.To = parseAddrs(toJSON)
	d.Cc = parseAddrs(ccJSON)

	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "route failed")
		return
	}
	text, html, atts, err := s.ensureBody(key, id, route)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch body failed: "+err.Error())
		return
	}
	d.Text = text
	cidURLs := map[string]string{}
	visible := []store.AttachmentRow{}
	for _, a := range atts {
		if a.ContentID != "" {
			cidURLs[a.ContentID] = "/api/messages/" + strconv.FormatInt(id, 10) +
				"/attachments/" + strconv.FormatInt(a.ID, 10)
		}
		if !a.Inline {
			visible = append(visible, a)
		}
	}
	d.HTML = sanitizeMailHTML(html, cidURLs)
	d.Attachments = visible
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDownloadAttachment(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	attID, err := strconv.ParseInt(r.PathValue("attId"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad attachment id")
		return
	}
	att, err := s.st.GetAttachment(id, attID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "attachment not found")
		return
	}
	if att.LocalPath != "" {
		if data, err := os.ReadFile(att.LocalPath); err == nil {
			filename := att.Filename
			if filename == "" {
				filename = "attachment"
			}
			ctype := att.Mime
			if ctype == "" {
				ctype = "application/octet-stream"
			}
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "route failed")
		return
	}
	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cl.Close()
	if _, err := cl.Select(route.Folder); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	raw, err := cl.FetchRaw(route.UID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	part, err := mail.ParseBodyPart(raw, att.PartPath)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	filename := att.Filename
	if filename == "" {
		filename = part.Filename
	}
	ctype := att.Mime
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Length", strconv.Itoa(len(part.Bytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(part.Bytes)
}

type flagsReq struct {
	Seen    *bool `json:"seen"`
	Flagged *bool `json:"flagged"`
}

func (s *Server) handleSetFlags(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	var req flagsReq
	if err := decodeJSON(r, &req); err != nil || (req.Seen == nil && req.Flagged == nil) {
		writeErr(w, http.StatusBadRequest, "seen or flagged required")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	flags, err := s.st.MessageFlags(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "flags failed")
		return
	}
	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cl.Close()
	if _, err := cl.SelectRW(route.Folder); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	apply := func(flag imap.Flag, bit uint32, val *bool) error {
		if val == nil {
			return nil
		}
		if err := cl.StoreFlag(route.UID, flag, *val); err != nil {
			return err
		}
		if *val {
			flags |= bit
		} else {
			flags &^= bit
		}
		return nil
	}
	if err := apply(imap.FlagSeen, mail.FlagSeen, req.Seen); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := apply(imap.FlagFlagged, mail.FlagFlagged, req.Flagged); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.st.SetLocalFlags(id, flags); err != nil {
		writeErr(w, http.StatusInternalServerError, "local flags failed")
		return
	}
	s.broadcastFlags([]int64{id})
	s.broadcastFolder(route.FolderID)
	writeJSON(w, http.StatusOK, map[string]uint32{"flags": flags})
}

type moveReq struct {
	To string `json:"to"`
}

func (s *Server) handleMoveMessage(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	var req moveReq
	if err := decodeJSON(r, &req); err != nil || req.To == "" {
		writeErr(w, http.StatusBadRequest, "destination special use required")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	destID, dest, ok, err := s.st.FolderBySpecial(route.AccountID, req.To)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "folder lookup failed")
		return
	}
	if !ok {
		writeErr(w, http.StatusBadRequest, "this account has no folder for "+req.To)
		return
	}
	if dest == route.Folder {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cl.Close()
	if _, err := cl.SelectRW(route.Folder); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	destUID, err := cl.MoveMessage(route.UID, dest)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if destUID > 0 {
		err = s.st.MoveLocalMessage(id, destID, destUID)
	} else {
		err = s.st.DeleteLocalMessage(id)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "local move failed")
		return
	}
	s.broadcastRemoved(route.FolderID, []int64{id})
	s.broadcastFolder(route.FolderID)
	s.broadcastFolder(destID)
	if destUID > 0 {
		var newID int64
		if err := s.st.DB.QueryRow("SELECT id FROM messages WHERE account_id = ? AND folder_id = ? AND uid = ?",
			route.AccountID, destID, destUID).Scan(&newID); err == nil && newID > 0 {
			s.broadcastMessages([]int64{newID})
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad message id")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "message not found")
		return
	}
	var special string
	_ = s.st.DB.QueryRow("SELECT special_use FROM folders WHERE id = ?", route.FolderID).Scan(&special)

	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cl.Close()
	if _, err := cl.SelectRW(route.Folder); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	permanent := special == "trash" || special == "junk"
	if permanent {
		if err := cl.DeleteMessage(route.UID); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if err := s.st.DeleteLocalMessage(id); err != nil {
			writeErr(w, http.StatusInternalServerError, "local delete failed")
			return
		}
		s.broadcastRemoved(route.FolderID, []int64{id})
		s.broadcastFolder(route.FolderID)
	} else {
		destID, dest, ok, err := s.st.FolderBySpecial(route.AccountID, "trash")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "folder lookup failed")
			return
		}
		if !ok {
			writeErr(w, http.StatusBadRequest, "this account has no trash folder")
			return
		}
		destUID, err := cl.MoveMessage(route.UID, dest)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if destUID > 0 {
			err = s.st.MoveLocalMessage(id, destID, destUID)
		} else {
			err = s.st.DeleteLocalMessage(id)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "local move failed")
			return
		}
		s.broadcastRemoved(route.FolderID, []int64{id})
		s.broadcastFolder(route.FolderID)
		s.broadcastFolder(destID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type markAllReadReq struct {
	Special  string `json:"special"`
	FolderID int64  `json:"folder_id"`
}

func (s *Server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req markAllReadReq
	if err := decodeJSON(r, &req); err != nil || (req.FolderID == 0 && req.Special == "") {
		writeErr(w, http.StatusBadRequest, "folder_id or special required")
		return
	}
	type folderRef struct {
		id        int64
		accountID int64
		name      string
	}
	var refs []folderRef
	if req.FolderID > 0 {
		var fr folderRef
		fr.id = req.FolderID
		err := s.st.DB.QueryRow("SELECT account_id, name FROM folders WHERE id = ?", req.FolderID).
			Scan(&fr.accountID, &fr.name)
		if err != nil {
			writeErr(w, http.StatusNotFound, "folder not found")
			return
		}
		refs = append(refs, fr)
	} else {
		rows, err := s.st.DB.Query("SELECT id, account_id, name FROM folders WHERE special_use = ?", req.Special)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "folder lookup failed")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var fr folderRef
			if err := rows.Scan(&fr.id, &fr.accountID, &fr.name); err == nil {
				refs = append(refs, fr)
			}
		}
		rows.Close()
	}
	updated := 0
	for _, fr := range refs {
		rows, err := s.st.DB.Query(`SELECT id, uid FROM messages
			WHERE folder_id = ? AND (flags & 1) = 0`, fr.id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "message lookup failed")
			return
		}
		var ids []int64
		var uids []uint32
		for rows.Next() {
			var mid int64
			var uid uint32
			if err := rows.Scan(&mid, &uid); err == nil {
				ids = append(ids, mid)
				uids = append(uids, uid)
			}
		}
		rows.Close()
		if len(uids) == 0 {
			continue
		}
		cl, err := s.openClient(key, fr.accountID)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		if _, err := cl.SelectRW(fr.name); err != nil {
			cl.Close()
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		for start := 0; start < len(uids); start += 500 {
			end := start + 500
			if end > len(uids) {
				end = len(uids)
			}
			if err := cl.StoreFlags(uids[start:end], imap.FlagSeen, true); err != nil {
				cl.Close()
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
		}
		cl.Close()
		if _, err := s.st.DB.Exec(`UPDATE messages SET flags = flags | 1
			WHERE folder_id = ? AND (flags & 1) = 0`, fr.id); err != nil {
			writeErr(w, http.StatusInternalServerError, "local update failed")
			return
		}
		_ = s.st.RefreshUnread(fr.id)
		updated += len(ids)
		s.broadcastFlags(ids)
		s.broadcastFolder(fr.id)
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

type attachmentIn struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	DataB64     string `json:"data_b64"`
}

type composeReq struct {
	AccountID   int64          `json:"account_id"`
	To          string         `json:"to"`
	Cc          string         `json:"cc"`
	Bcc         string         `json:"bcc"`
	Subject     string         `json:"subject"`
	Text        string         `json:"text"`
	HTML        string         `json:"html"`
	ReplyToID   int64          `json:"reply_to_id"`
	DraftID     int64          `json:"draft_id"`
	Attachments []attachmentIn `json:"attachments"`
}

type outgoing struct {
	acct      mail.Account
	raw       []byte
	rcpts     []string
	from      mail.Addr
	to        []mail.Addr
	cc        []mail.Addr
	subject   string
	text      string
	html      string
	messageID string
	inReplyTo string
}

func parseAddrList(s string) ([]mail.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	list, err := netmail.ParseAddressList(s)
	if err != nil {
		return nil, err
	}
	out := make([]mail.Addr, 0, len(list))
	for _, a := range list {
		out = append(out, mail.Addr{Name: a.Name, Email: a.Address})
	}
	return out, nil
}

func (s *Server) prepareOutgoing(key []byte, req composeReq, requireRcpts bool) (*outgoing, error) {
	acct, err := s.loadMailAccount(req.AccountID, key)
	if err != nil {
		return nil, err
	}
	to, err := parseAddrList(req.To)
	if err != nil {
		return nil, fmt.Errorf("收件人格式不对: %w", err)
	}
	cc, err := parseAddrList(req.Cc)
	if err != nil {
		return nil, fmt.Errorf("抄送格式不对: %w", err)
	}
	bcc, err := parseAddrList(req.Bcc)
	if err != nil {
		return nil, fmt.Errorf("密送格式不对: %w", err)
	}
	rcpts := []string{}
	for _, group := range [][]mail.Addr{to, cc, bcc} {
		for _, a := range group {
			rcpts = append(rcpts, a.Email)
		}
	}
	if requireRcpts && len(rcpts) == 0 {
		return nil, fmt.Errorf("至少填写一个收件人")
	}
	from := mail.Addr{Name: acct.FromName, Email: acct.Email}

	attachments := make([]mail.OutgoingAttachment, 0, len(req.Attachments))
	totalAttach := 0
	for _, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataB64)
		if err != nil {
			return nil, fmt.Errorf("附件 %s 的数据无效", a.Filename)
		}
		totalAttach += len(data)
		if int64(totalAttach) > s.st.AppSettings().MaxAttachmentMB<<20 {
			return nil, fmt.Errorf("附件总大小超过 %d MB 上限", s.st.AppSettings().MaxAttachmentMB)
		}
		attachments = append(attachments, mail.OutgoingAttachment{
			Filename: a.Filename, ContentType: a.ContentType, Data: data,
		})
	}

	var inReplyTo string
	if req.ReplyToID > 0 {
		var mid string
		if err := s.st.DB.QueryRow("SELECT message_id FROM messages WHERE id = ?", req.ReplyToID).Scan(&mid); err == nil {
			mid = strings.Trim(mid, "<>")
			if mid != "" {
				inReplyTo = "<" + mid + ">"
			}
		}
	}
	domain := acct.Email
	if i := strings.LastIndex(acct.Email, "@"); i >= 0 {
		domain = acct.Email[i+1:]
	}
	rb := make([]byte, 6)
	_, _ = rand.Read(rb)
	messageID := fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(rb), domain)

	htmlBody := ""
	if strings.TrimSpace(req.HTML) != "" {
		htmlBody = mailPolicy.Sanitize(req.HTML)
	}
	raw, err := mail.BuildMessage(mail.ComposeInput{
		From:        from,
		To:          to,
		Cc:          cc,
		Subject:     req.Subject,
		Text:        req.Text,
		HTML:        htmlBody,
		Date:        time.Now(),
		MessageID:   messageID,
		InReplyTo:   inReplyTo,
		References:  inReplyTo,
		Attachments: attachments,
	})
	if err != nil {
		return nil, err
	}
	return &outgoing{
		acct: acct, raw: raw, rcpts: rcpts, from: from, to: to, cc: cc,
		subject: req.Subject, text: req.Text, html: htmlBody, messageID: strings.Trim(messageID, "<>"),
		inReplyTo: strings.Trim(inReplyTo, "<>"),
	}, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func (s *Server) appendCopy(key []byte, accountID int64, special string,
	imapFlags []imap.Flag, localFlags uint32, out *outgoing) (int64, error) {
	folderID, folderName, ok, err := s.st.FolderBySpecial(accountID, special)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("该账号没有 %s 文件夹", special)
	}
	cl, err := s.openClient(key, accountID)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	uid, err := cl.AppendMessage(folderName, out.raw, imapFlags...)
	if err != nil {
		return 0, err
	}
	row := &store.MessageRow{
		UID:       uid,
		MessageID: out.messageID,
		ThreadKey: s.st.ComputeThreadKey(accountID, out.messageID, out.inReplyTo),
		Subject:   out.subject,
		FromJSON:  mustJSON([]mail.Addr{out.from}),
		ToJSON:    mustJSON(out.to),
		CcJSON:    mustJSON(out.cc),
		DateTS:    time.Now().Unix(),
		Flags:     localFlags,
		Size:      int64(len(out.raw)),
	}
	id, _, err := s.st.UpsertMessage(accountID, folderID, row)
	if err != nil {
		return 0, err
	}
	if err := s.st.SaveBody(id, out.text, out.html, mail.MakeSnippet(out.text, out.html)); err != nil {
		return 0, err
	}
	_, _ = s.st.DB.Exec("UPDATE folders SET total = total + 1 WHERE id = ?", folderID)
	_ = s.st.RefreshUnread(folderID)
	return id, nil
}

func (s *Server) deleteMessageEverywhere(key []byte, messageID int64) error {
	route, err := s.st.MessageRoute(messageID)
	if err != nil {
		return err
	}
	cl, err := s.openClient(key, route.AccountID)
	if err != nil {
		return err
	}
	defer cl.Close()
	if _, err := cl.SelectRW(route.Folder); err != nil {
		return err
	}
	if err := cl.DeleteMessage(route.UID); err != nil {
		return err
	}
	return s.st.DeleteLocalMessage(messageID)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req composeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	out, err := s.prepareOutgoing(key, req, true)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := mail.SendSMTP(out.acct, out.acct.Email, out.rcpts, out.raw); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	warnings := []string{}
	if sentID, err := s.appendCopy(key, req.AccountID, "sent",
		[]imap.Flag{imap.FlagSeen}, mail.FlagSeen, out); err != nil {
		warnings = append(warnings, "邮件已发送，但存入「已发送」失败："+err.Error())
	} else {
		s.broadcastMessages([]int64{sentID})
		if fid, _, ok, _ := s.st.FolderBySpecial(req.AccountID, "sent"); ok {
			s.broadcastFolder(fid)
		}
	}
	if req.DraftID > 0 {
		route, rerr := s.st.MessageRoute(req.DraftID)
		if err := s.deleteMessageEverywhere(key, req.DraftID); err != nil {
			warnings = append(warnings, "邮件已发送，但原草稿删除失败："+err.Error())
		} else if rerr == nil {
			s.broadcastRemoved(route.FolderID, []int64{req.DraftID})
			s.broadcastFolder(route.FolderID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": strings.Join(warnings, "；")})
}

func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req composeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	out, err := s.prepareOutgoing(key, req, false)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.appendCopy(key, req.AccountID, "drafts",
		[]imap.Flag{imap.FlagDraft}, mail.FlagSeen|mail.FlagDraft, out)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "存草稿失败："+err.Error())
		return
	}
	s.broadcastMessages([]int64{id})
	warning := ""
	if req.DraftID > 0 && req.DraftID != id {
		route, rerr := s.st.MessageRoute(req.DraftID)
		if err := s.deleteMessageEverywhere(key, req.DraftID); err != nil {
			warning = "新草稿已保存，但旧草稿删除失败：" + err.Error()
		} else if rerr == nil {
			s.broadcastRemoved(route.FolderID, []int64{req.DraftID})
		}
	}
	if fid, _, ok, _ := s.st.FolderBySpecial(req.AccountID, "drafts"); ok {
		s.broadcastFolder(fid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "warning": warning})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []messageOut{})
		return
	}
	limit := 60
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	rows, err := s.st.DB.Query(`SELECT m.id, m.account_id, a.label, m.folder_id, f.name, m.subject,
		m.from_json, m.date_ts, m.flags, m.has_attach, m.snippet, m.size
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN accounts a ON a.id = m.account_id
		JOIN folders f ON f.id = m.folder_id
		WHERE messages_fts MATCH ? ORDER BY m.date_ts DESC LIMIT ?`,
		store.BuildFTSQuery(q), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search failed")
		return
	}
	defer rows.Close()
	out := []messageOut{}
	for rows.Next() {
		var row messageOut
		var fromJSON string
		var hasAttach int
		if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountLabel, &row.FolderID, &row.FolderName,
			&row.Subject, &fromJSON, &row.DateTS, &row.Flags, &hasAttach, &row.Snippet, &row.Size); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan failed")
			return
		}
		row.HasAttach = hasAttach != 0
		if from := parseAddrs(fromJSON); len(from) > 0 {
			row.FromName = from[0].Name
			row.FromEmail = from[0].Email
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

type contactOut struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	found := map[string]string{}
	add := func(addrs []mail.Addr) {
		for _, a := range addrs {
			email := strings.ToLower(strings.TrimSpace(a.Email))
			if email == "" || !strings.Contains(email, "@") {
				continue
			}
			if _, ok := found[email]; !ok || found[email] == "" {
				found[email] = a.Name
			}
		}
	}
	rows, err := s.st.DB.Query("SELECT from_json, to_json, cc_json FROM messages")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "contacts failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var fromJSON, toJSON, ccJSON string
		if err := rows.Scan(&fromJSON, &toJSON, &ccJSON); err != nil {
			continue
		}
		add(parseAddrs(fromJSON))
		add(parseAddrs(toJSON))
		add(parseAddrs(ccJSON))
	}
	rows.Close()
	arows, err := s.st.DB.Query("SELECT label, email FROM accounts")
	if err == nil {
		defer arows.Close()
		for arows.Next() {
			var label, email string
			if err := arows.Scan(&label, &email); err == nil {
				add([]mail.Addr{{Name: label, Email: email}})
			}
		}
	}
	out := make([]contactOut, 0, len(found))
	for email, name := range found {
		out = append(out, contactOut{Name: name, Email: email})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name+out[i].Email), strings.ToLower(out[j].Name+out[j].Email)
		return a < b
	})
	if len(out) > 500 {
		out = out[:500]
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.uiFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "no such endpoint")
			return
		}
		f, err := s.uiFS.Open(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		f.Close()
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) handleGetAppSettings(w http.ResponseWriter, r *http.Request) {
	used, err := s.st.CacheUsedBytes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cache usage failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":           s.st.AppSettings(),
		"cache_used_bytes":   used,
		"cache_budget_bytes": s.st.CacheBudgetBytes(),
	})
}

func (s *Server) handlePutAppSettings(w http.ResponseWriter, r *http.Request) {
	var req store.AppSettings
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad settings")
		return
	}
	if req.CacheBudgetGB < 0 || req.CacheBudgetGB > 1024 ||
		req.SyncBatch < 1 || req.SyncBatch > 1000 ||
		req.MaxAttachmentMB < 1 || req.MaxAttachmentMB > 200 ||
		req.PushWindowMinutes < 0 || req.PushWindowMinutes > 43200 {
		writeErr(w, http.StatusBadRequest, "settings out of range")
		return
	}
	if err := s.st.SaveAppSettings(req); err != nil {
		writeErr(w, http.StatusInternalServerError, "save settings failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
