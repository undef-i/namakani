package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
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
	"namakani/internal/proton"
	"namakani/internal/store"
	syncmgr "namakani/internal/sync"
	"namakani/internal/translate"
	"namakani/internal/tuta"

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
	drainWake     chan struct{}
	drainMu       sync.Mutex
	drainRunning  map[int64]bool
	connFailMu    sync.Mutex
	connFail      map[int64]*connFailState
	drainOpen     func(ctx context.Context, key []byte, accountID int64) (mail.Conn, error)
	appendOpen    func(key []byte, accountID int64) (mail.Conn, error)
	sendDeliver   func(key []byte, out *outgoing) (string, error)
	sendFileCopy  func(key []byte, out *outgoing, sentID string) (int64, error)
	loginMu       sync.Mutex
	loginFails    int
	loginLastFail time.Time
	tgSeenMu      sync.Mutex
	tgSeen        map[string]tgChat
	tgMu          sync.Mutex
	tgCancel      context.CancelFunc
	tgRunning     string
	protonAuthMu  sync.Mutex
	protonAuth    map[string]protonAuthEntry
	trMgr         *translate.Manager
	translateBuild func(cfg translateSettings) (translate.Engine, error)
	avatarResolve func(ctx context.Context, email string) (string, string, []byte)
}

type protonAuthEntry struct {
	bridge string
	at     time.Time
}

func New(st *store.Store, uiFS fs.FS, mgr *syncmgr.Manager) *Server {
	s := &Server{st: st, uiFS: uiFS, mgr: mgr, hub: newHub(), keys: make(map[string][]byte), tgSeen: make(map[string]tgChat), protonAuth: make(map[string]protonAuthEntry), drainWake: make(chan struct{}, 1), drainRunning: make(map[int64]bool), connFail: make(map[int64]*connFailState), trMgr: translate.NewManager(filepath.Join(st.DataDir(), "models"))}
	s.drainOpen = s.openBackgroundConn
	s.appendOpen = s.openConn
	go st.WarmBodyBytes()
	s.tgReconcile()
	go s.drainLoop()
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
	mux.HandleFunc("POST /api/accounts/test", s.authOnly(s.handleTestAccountForm))
	mux.HandleFunc("PUT /api/accounts/{id}", s.authOnly(s.handleUpdateAccount))
	mux.HandleFunc("DELETE /api/accounts/{id}", s.authOnly(s.handleDeleteAccount))
	mux.HandleFunc("POST /api/accounts/{id}/test", s.authOnly(s.handleTestAccount))
	mux.HandleFunc("GET /api/accounts/{id}/sync", s.authOnly(s.handleSyncStatus))
	mux.HandleFunc("POST /api/accounts/{id}/reconnect", s.authOnly(s.handleReconnectAccount))
	mux.HandleFunc("GET /api/folders", s.authOnly(s.handleListFolders))
	mux.HandleFunc("POST /api/folders/{id}/purge", s.authOnly(s.handlePurgeFolder))
	mux.HandleFunc("GET /api/messages", s.authOnly(s.handleListMessages))
	mux.HandleFunc("GET /api/messages/{id}", s.authOnly(s.handleGetMessage))
	mux.HandleFunc("GET /api/messages/{id}/attachments/{attId}", s.authOnly(s.handleDownloadAttachment))
	mux.HandleFunc("POST /api/messages/{id}/flags", s.authOnly(s.handleSetFlags))
	mux.HandleFunc("POST /api/messages/flags", s.authOnly(s.handleBatchFlags))
	mux.HandleFunc("POST /api/messages/{id}/move", s.authOnly(s.handleMoveMessage))
	mux.HandleFunc("POST /api/messages/{id}/restore", s.authOnly(s.handleRestoreMessage))
	mux.HandleFunc("POST /api/messages/{id}/delete", s.authOnly(s.handleDeleteMessage))
	mux.HandleFunc("POST /api/send", s.authOnly(s.handleSend))
	mux.HandleFunc("POST /api/send/schedule", s.authOnly(s.handleScheduleSend))
	mux.HandleFunc("GET /api/scheduled", s.authOnly(s.handleListScheduled))
	mux.HandleFunc("DELETE /api/scheduled/{id}", s.authOnly(s.handleCancelScheduled))
	mux.HandleFunc("GET /api/scheduled/{id}", s.authOnly(s.handleGetScheduled))
	mux.HandleFunc("PUT /api/scheduled/{id}", s.authOnly(s.handleUpdateScheduled))
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
	mux.HandleFunc("POST /api/telegram/webapp-auth", s.handleTelegramWebAppAuth)
	mux.HandleFunc("GET /api/settings/translate", s.authOnly(s.handleGetTranslateSettings))
	mux.HandleFunc("PUT /api/settings/translate", s.authOnly(s.handlePutTranslateSettings))
	mux.HandleFunc("POST /api/messages/{id}/translate", s.authOnly(s.handleTranslateMessage))
	mux.HandleFunc("GET /api/translate/models", s.authOnly(s.handleTranslateModels))
	mux.HandleFunc("POST /api/translate/llm/models", s.authOnly(s.handleLLMModels))
	mux.HandleFunc("POST /api/avatars", s.authOnly(s.handleAvatars))
	mux.HandleFunc("GET /api/avatar/img", s.authOnly(s.handleAvatarImage))
	mux.HandleFunc("GET /api/settings/webpush", s.authOnly(s.handleGetWebPush))
	mux.HandleFunc("POST /api/settings/webpush/test", s.authOnly(s.handleWebPushTest))
	mux.HandleFunc("POST /api/push/subscribe", s.authOnly(s.handlePushSubscribe))
	mux.HandleFunc("POST /api/push/unsubscribe", s.authOnly(s.handlePushUnsubscribe))
	mux.HandleFunc("POST /api/messages/mark-all-read", s.authOnly(s.handleMarkAllRead))
	mux.HandleFunc("POST /api/ops/{id}/undo", s.authOnly(s.handleUndoOp))
	mux.HandleFunc("POST /api/ops/{id}/retry", s.authOnly(s.handleRetryOp))
	mux.HandleFunc("DELETE /api/ops/{id}", s.authOnly(s.handleDiscardOp))
	mux.HandleFunc("POST /api/ops/{id}/send-now", s.authOnly(s.handleSendNowOp))
	mux.HandleFunc("GET /api/ops/attention", s.authOnly(s.handleListAttention))
	mux.HandleFunc("GET /api/ops/pending", s.authOnly(s.handleListPending))
	mux.HandleFunc("POST /api/ops/by-keys", s.authOnly(s.handleOpsByKeys))
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

type userFacingError struct{ s string }

func (e *userFacingError) Error() string { return e.s }

func userFacing(s string) error { return &userFacingError{s: s} }

func describeConnError(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "authenticationfailed"), strings.Contains(s, "authentication failed"),
		strings.Contains(s, "invalid credentials"), strings.Contains(s, "login failed"):
		return "登录被拒绝，请检查用户名和密码"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline exceeded"):
		return "连接超时，请检查服务器地址和网络"
	case strings.Contains(s, "certificate"), strings.Contains(s, "tls"):
		return "TLS 证书校验失败"
	case strings.Contains(s, "quota"):
		return "空间或配额不足"
	default:
		return "无法连接服务器，请检查地址和端口"
	}
}

func writeUserErr(w http.ResponseWriter, code int, fallback string, err error) {
	var ue *userFacingError
	if errors.As(err, &ue) {
		writeErr(w, code, ue.s)
		return
	}
	if errors.Is(err, store.ErrClientKeyConflict) {
		writeErr(w, http.StatusConflict, "重复提交被拒绝，请刷新后确认当前状态")
		return
	}
	log.Printf("namakani: %s: %v", fallback, err)
	writeErr(w, code, fallback)
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
		if _, expiresAt, exists, err := auth.SessionInfo(s.st, token); err == nil && exists && time.Now().Unix() <= expiresAt {
			return key, true
		}
		s.mu.Lock()
		delete(s.keys, token)
		s.mu.Unlock()
		return nil, false
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
			writeErr(w, http.StatusUnauthorized, "尚未登录")
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
		writeErr(w, http.StatusInternalServerError, "读取状态失败")
		return
	}
	_, authed := s.credentialKey(r)
	if authed {
		s.renewSession(w, r)
	}
	out := map[string]any{"setup": setup, "authed": authed, "version": BuildVersion}
	if authed {
		out["app_settings"] = s.st.AppSettings()
		if n, err := s.st.AttentionCount(); err == nil {
			out["attention"] = n
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type passwordReq struct {
	Password string `json:"password"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	setup, err := auth.IsSetup(s.st)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取状态失败")
		return
	}
	if setup {
		writeErr(w, http.StatusConflict, "已完成初始化")
		return
	}
	var req passwordReq
	if err := decodeJSON(r, &req); err != nil || len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "密码至少 8 位")
		return
	}
	if err := auth.SetupMaster(s.st, req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, "初始化失败")
		return
	}
	key, ok, err := auth.VerifyMaster(s.st, req.Password)
	if err != nil || !ok {
		writeErr(w, http.StatusInternalServerError, "初始化校验失败")
		return
	}
	if err := s.startSession(w, r, key); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话处理失败")
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
		writeErr(w, http.StatusBadRequest, "请填写密码")
		return
	}
	if s.loginWait() > 0 {
		writeErr(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	key, ok, err := auth.VerifyMaster(s.st, req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "登录失败")
		return
	}
	if !ok {
		s.noteLoginFailure()
		writeErr(w, http.StatusUnauthorized, "密码不对")
		return
	}
	s.resetLoginFailures()
	if err := s.startSession(w, r, key); err != nil {
		writeErr(w, http.StatusInternalServerError, "会话处理失败")
		return
	}
	s.startAllWorkers(key)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := s.sessionToken(r)
	_ = auth.DeleteSession(s.st, token)
	s.mu.Lock()
	delete(s.keys, token)
	s.mu.Unlock()
	if n, err := auth.LiveSessionCount(s.st); err == nil && n == 0 {
		s.mgr.StopAll()
		s.mu.Lock()
		s.keys = make(map[string][]byte)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "wm_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type accountIn struct {
	Label           string `json:"label"`
	Email           string `json:"email"`
	ImapHost        string `json:"imap_host"`
	ImapPort        any    `json:"imap_port"`
	ImapTLS         string `json:"imap_tls"`
	SmtpHost        string `json:"smtp_host"`
	SmtpPort        any    `json:"smtp_port"`
	SmtpTLS         string `json:"smtp_tls"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	Signature       string `json:"signature"`
	FromName        string `json:"from_name"`
	TLSSkipVerify   bool   `json:"tls_skip_verify"`
	Kind            string `json:"kind"`
	TOTP            string `json:"totp"`
	MailboxPassword string `json:"mailbox_password"`
	CaptchaToken    string `json:"captcha_token"`
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
	Kind          string `json:"kind"`
}

func validateAccount(in *accountIn, passwordRequired bool) (imapPort, smtpPort int, err error) {
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" ||
		strings.TrimSpace(in.ImapHost) == "" || strings.TrimSpace(in.SmtpHost) == "" ||
		strings.TrimSpace(in.Username) == "" {
		return 0, 0, userFacing("请填写名称、邮箱、服务器地址和用户名")
	}
	if passwordRequired && in.Password == "" {
		return 0, 0, userFacing("请填写密码")
	}
	if !auth.ValidTLSMode(in.ImapTLS) || !auth.ValidTLSMode(in.SmtpTLS) {
		return 0, 0, userFacing("加密方式只能是 tls、starttls 或 none")
	}
	imapPort, err = auth.ParsePort(in.ImapPort)
	if err != nil {
		return 0, 0, userFacing(err.Error())
	}
	smtpPort, err = auth.ParsePort(in.SmtpPort)
	if err != nil {
		return 0, 0, userFacing(err.Error())
	}
	return imapPort, smtpPort, nil
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT id, label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, signature, from_name, tls_skip_verify, kind FROM accounts ORDER BY sort, id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "列表加载失败")
		return
	}
	defer rows.Close()
	out := []accountOut{}
	for rows.Next() {
		var a accountOut
		if err := rows.Scan(&a.ID, &a.Label, &a.Email, &a.ImapHost, &a.ImapPort, &a.ImapTLS,
			&a.SmtpHost, &a.SmtpPort, &a.SmtpTLS, &a.Username, &a.Signature, &a.FromName, &a.TLSSkipVerify, &a.Kind); err != nil {
			writeErr(w, http.StatusInternalServerError, "列表加载失败")
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
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if in.Kind == "proton" {
		s.createProtonAccount(w, key, &in)
		return
	}
	if in.Kind == "tuta" {
		s.createTutaAccount(w, key, &in)
		return
	}
	imapPort, smtpPort, err := validateAccount(&in, true)
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "账号信息不完整，请检查后重试", err)
		return
	}
	enc, err := auth.Encrypt(key, in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密失败")
		return
	}
	res, err := s.st.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, signature, from_name, tls_skip_verify, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
		in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, enc, in.Signature, in.FromName, in.TLSSkipVerify, time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
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
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if in.Kind == "proton" {
		s.updateProtonAccount(w, r, key, &in)
		return
	}
	if in.Kind == "tuta" {
		s.updateTutaAccount(w, r, key, &in)
		return
	}
	imapPort, smtpPort, err := validateAccount(&in, false)
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "账号信息不完整，请检查后重试", err)
		return
	}
	id := r.PathValue("id")
	if in.Password != "" {
		enc, err := auth.Encrypt(key, in.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密失败")
			return
		}
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, imap_host=?, imap_port=?, imap_tls=?,
			smtp_host=?, smtp_port=?, smtp_tls=?, username=?, enc_password=?, signature=?, from_name=?, tls_skip_verify=? WHERE id=?`,
			in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
			in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, enc, in.Signature, in.FromName, in.TLSSkipVerify, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
	} else {
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, imap_host=?, imap_port=?, imap_tls=?,
			smtp_host=?, smtp_port=?, smtp_tls=?, username=?, signature=?, from_name=?, tls_skip_verify=? WHERE id=?`,
			in.Label, in.Email, in.ImapHost, imapPort, in.ImapTLS,
			in.SmtpHost, smtpPort, in.SmtpTLS, in.Username, in.Signature, in.FromName, in.TLSSkipVerify, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
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
	idNum, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "账号编号不对")
		return
	}
	s.mgr.Stop(idNum)
	if err := s.st.DeleteAccount(idNum); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

const protonAuthReuseTTL = 10 * time.Minute

func protonAuthFingerprint(in *accountIn) string {
	sum := sha256.Sum256([]byte(in.Email + "\x00" + in.Password + "\x00" + in.MailboxPassword + "\x00" + in.TOTP))
	return hex.EncodeToString(sum[:])
}

func (s *Server) takeProtonAuth(in *accountIn) (string, bool) {
	fp := protonAuthFingerprint(in)
	s.protonAuthMu.Lock()
	defer s.protonAuthMu.Unlock()
	e, ok := s.protonAuth[fp]
	if !ok {
		return "", false
	}
	delete(s.protonAuth, fp)
	if time.Since(e.at) > protonAuthReuseTTL {
		return "", false
	}
	return e.bridge, true
}

func (s *Server) stashProtonAuth(in *accountIn, bridge string) {
	s.protonAuthMu.Lock()
	s.protonAuth[protonAuthFingerprint(in)] = protonAuthEntry{bridge: bridge, at: time.Now()}
	s.protonAuthMu.Unlock()
}

func (s *Server) authorizeProton(in *accountIn) (string, error) {
	if bridge, ok := s.takeProtonAuth(in); ok {
		return bridge, nil
	}
	bridge, err := proton.Authorize(in.Email, in.Password, in.MailboxPassword, in.TOTP, in.CaptchaToken)
	if err == nil {
		s.stashProtonAuth(in, bridge)
	}
	return bridge, err
}

func (s *Server) createProtonAccount(w http.ResponseWriter, key []byte, in *accountIn) {
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称、邮箱和密码")
		return
	}
	bridgePassword, err := s.authorizeProton(in)
	if err == proton.ErrTOTPRequired {
		writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
		return
	}
	var capErr *proton.CaptchaError
	if errors.As(err, &capErr) {
		writeJSON(w, http.StatusOK, map[string]any{"needs_captcha": true, "captcha_url": capErr.URL, "captcha_token": capErr.Token})
		return
	}
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱和密码", err)
		return
	}
	enc, err := auth.Encrypt(key, bridgePassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密失败")
		return
	}
	username := in.Username
	if username == "" {
		username = in.Email
	}
	res, err := s.st.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, signature, from_name, tls_skip_verify, kind, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Label, in.Email, "", 0, "none",
		"", 0, "none", username, enc, in.Signature, in.FromName, false, "proton", time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	id, _ := res.LastInsertId()
	s.startWorkerFor(id, key)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) updateProtonAccount(w http.ResponseWriter, r *http.Request, key []byte, in *accountIn) {
	id := r.PathValue("id")
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称和邮箱")
		return
	}
	username := in.Username
	if username == "" {
		username = in.Email
	}
	if in.Password != "" {
		bridgePassword, err := s.authorizeProton(in)
		if err == proton.ErrTOTPRequired {
			writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
			return
		}
		var capErr *proton.CaptchaError
		if errors.As(err, &capErr) {
			writeJSON(w, http.StatusOK, map[string]any{"needs_captcha": true, "captcha_url": capErr.URL, "captcha_token": capErr.Token})
			return
		}
		if err != nil {
			writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱和密码", err)
			return
		}
		enc, err := auth.Encrypt(key, bridgePassword)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密失败")
			return
		}
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, username=?, enc_password=?, signature=?, from_name=? WHERE id=?`,
			in.Label, in.Email, username, enc, in.Signature, in.FromName, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
	} else {
		_, err := s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, username=?, signature=?, from_name=? WHERE id=?`,
			in.Label, in.Email, username, in.Signature, in.FromName, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
	}
	if idNum, err := strconv.ParseInt(id, 10, 64); err == nil {
		s.mgr.Stop(idNum)
		s.startWorkerFor(idNum, key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) createTutaAccount(w http.ResponseWriter, key []byte, in *accountIn) {
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" || in.Password == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称、邮箱和密码")
		return
	}
	start := time.Now()
	userID, accessToken, err := tuta.Authorize(in.Email, in.Password, in.TOTP)
	if err == tuta.ErrTOTPRequired {
		writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
		return
	}
	if err != nil {
		log.Printf("tuta: create account authorize failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
		writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱、密码和两步验证码", err)
		return
	}
	log.Printf("tuta: create account authorize ok in %s", time.Since(start).Round(time.Millisecond))
	enc, err := auth.Encrypt(key, in.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密失败")
		return
	}
	username := in.Username
	if username == "" {
		username = in.Email
	}
	res, err := s.st.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, signature, from_name, tls_skip_verify, kind, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Label, in.Email, "", 0, "none",
		"", 0, "none", username, enc, in.Signature, in.FromName, false, "tuta", time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	id, _ := res.LastInsertId()
	if err := s.st.SaveTutaSession(id, userID, accessToken); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存会话失败")
		return
	}
	s.startWorkerFor(id, key)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) updateTutaAccount(w http.ResponseWriter, r *http.Request, key []byte, in *accountIn) {
	id := r.PathValue("id")
	if strings.TrimSpace(in.Label) == "" || strings.TrimSpace(in.Email) == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称和邮箱")
		return
	}
	username := in.Username
	if username == "" {
		username = in.Email
	}
	idNum, _ := strconv.ParseInt(id, 10, 64)
	if in.Password != "" {
		start := time.Now()
		userID, accessToken, err := tuta.Authorize(in.Email, in.Password, in.TOTP)
		if err == tuta.ErrTOTPRequired {
			writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
			return
		}
		if err != nil {
			log.Printf("tuta: update account %d authorize failed after %s: %v", idNum, time.Since(start).Round(time.Millisecond), err)
			writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱、密码和两步验证码", err)
			return
		}
		log.Printf("tuta: update account %d authorize ok in %s", idNum, time.Since(start).Round(time.Millisecond))
		enc, err := auth.Encrypt(key, in.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密失败")
			return
		}
		_, err = s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, username=?, enc_password=<redacted>, signature=?, from_name=? WHERE id=?`,
			in.Label, in.Email, username, enc, in.Signature, in.FromName, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
		if idNum > 0 {
			_ = s.st.SaveTutaSession(idNum, userID, accessToken)
		}
	} else {
		_, err := s.st.DB.Exec(`UPDATE accounts SET label=?, email=?, username=?, signature=?, from_name=? WHERE id=?`,
			in.Label, in.Email, username, in.Signature, in.FromName, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
	}
	if idNum > 0 {
		s.mgr.Stop(idNum)
		s.startWorkerFor(idNum, key)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) loadMailAccount(id int64, key []byte) (mail.Account, error) {
	var a mail.Account
	var enc string
	err := s.st.DB.QueryRow(`SELECT label, from_name, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, tls_skip_verify, kind FROM accounts WHERE id = ?`, id).
		Scan(&a.Label, &a.FromName, &a.Email, &a.ImapHost, &a.ImapPort, &a.ImapTLS,
			&a.SMTPHost, &a.SMTPPort, &a.SMTPTLS, &a.Username, &enc, &a.TLSSkipVerify, &a.Kind)
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

type accountTestIn struct {
	accountIn
	ID int64 `json:"id"`
}

type testSideResult struct {
	OK           bool     `json:"ok"`
	Folders      int      `json:"folders,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Error        string   `json:"error,omitempty"`
}

func (s *Server) handleTestAccountForm(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var in accountTestIn
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if in.Kind == "proton" {
		s.testProtonAccount(w, key, &in)
		return
	}
	if in.Kind == "tuta" {
		s.testTutaAccount(w, key, &in)
		return
	}
	s.testIMAPAccount(w, key, &in)
}

func (s *Server) testIMAPAccount(w http.ResponseWriter, key []byte, in *accountTestIn) {
	if strings.TrimSpace(in.ImapHost) == "" || strings.TrimSpace(in.SmtpHost) == "" ||
		strings.TrimSpace(in.Username) == "" {
		writeErr(w, http.StatusBadRequest, "请填写服务器地址和用户名")
		return
	}
	if !auth.ValidTLSMode(in.ImapTLS) || !auth.ValidTLSMode(in.SmtpTLS) {
		writeErr(w, http.StatusBadRequest, "加密方式只能是 tls、starttls 或 none")
		return
	}
	imapPort, err := auth.ParsePort(in.ImapPort)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	smtpPort, err := auth.ParsePort(in.SmtpPort)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	password := in.Password
	if password == "" && in.ID > 0 {
		stored, err := s.loadMailAccount(in.ID, key)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "保存的登录凭据不可用，请重新登录")
			return
		}
		password = stored.Password
	}
	if password == "" {
		writeErr(w, http.StatusBadRequest, "请填写密码")
		return
	}
	acct := mail.Account{
		Label:         in.Label,
		Email:         in.Email,
		ImapHost:      in.ImapHost,
		ImapPort:      imapPort,
		ImapTLS:       in.ImapTLS,
		SMTPHost:      in.SmtpHost,
		SMTPPort:      smtpPort,
		SMTPTLS:       in.SmtpTLS,
		Username:      in.Username,
		Password:      password,
		TLSSkipVerify: in.TLSSkipVerify,
	}
	imapRes := testSideResult{}
	if cl, err := mail.Connect(acct, nil); err != nil {
		log.Printf("namakani: test imap connect: %v", err)
		imapRes.Error = describeConnError(err)
	} else {
		folders, ferr := cl.ListFolders()
		caps := cl.Capabilities()
		cl.Close()
		if ferr != nil {
			log.Printf("namakani: test imap list folders: %v", ferr)
			imapRes.Error = "读取文件夹列表失败"
		} else {
			imapRes.OK = true
			imapRes.Folders = len(folders)
			imapRes.Capabilities = caps
		}
	}
	smtpRes := testSideResult{}
	if err := mail.TestSMTP(acct); err != nil {
		log.Printf("namakani: test smtp: %v", err)
		smtpRes.Error = describeConnError(err)
	} else {
		smtpRes.OK = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"imap": imapRes, "smtp": smtpRes})
}

func (s *Server) testProtonAccount(w http.ResponseWriter, key []byte, in *accountTestIn) {
	if strings.TrimSpace(in.Email) == "" {
		writeErr(w, http.StatusBadRequest, "请填写邮箱")
		return
	}
	if in.Password == "" {
		if in.ID <= 0 {
			writeErr(w, http.StatusBadRequest, "请填写密码")
			return
		}
		s.testProtonStored(w, key, in.ID)
		return
	}
	bridgePassword, err := s.authorizeProton(&in.accountIn)
	if err != nil {
		if err == proton.ErrTOTPRequired {
			writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
			return
		}
		var capErr *proton.CaptchaError
		if errors.As(err, &capErr) {
			writeJSON(w, http.StatusOK, map[string]any{"needs_captcha": true, "captcha_url": capErr.URL, "captcha_token": capErr.Token})
			return
		}
		writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱和密码", err)
		return
	}
	acct := mail.Account{Kind: "proton", Email: in.Email, Username: in.Email, Password: bridgePassword}
	cl, err := proton.Connect(s.st, acct)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "连接失败，请检查服务器设置和账号密码", err)
		return
	}
	defer cl.Close()
	folders, err := cl.ListFolders()
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "读取文件夹列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders), "capabilities": cl.Capabilities()})
}

func (s *Server) testProtonStored(w http.ResponseWriter, key []byte, id int64) {
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存的登录凭据不可用，请重新登录")
		return
	}
	cl, err := proton.Connect(s.st, acct)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "连接失败，请检查服务器设置和账号密码", err)
		return
	}
	defer cl.Close()
	folders, err := cl.ListFolders()
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "读取文件夹列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders), "capabilities": cl.Capabilities()})
}

func (s *Server) testTutaAccount(w http.ResponseWriter, key []byte, in *accountTestIn) {
	if strings.TrimSpace(in.Email) == "" {
		writeErr(w, http.StatusBadRequest, "请填写邮箱")
		return
	}
	if in.Password == "" {
		if in.ID <= 0 {
			writeErr(w, http.StatusBadRequest, "请填写密码")
			return
		}
		s.testTutaStored(w, key, in.ID)
		return
	}
	start := time.Now()
	cl, err := tuta.LoginSession(in.Email, in.Password, in.TOTP)
	if err != nil {
		if err == tuta.ErrTOTPRequired {
			writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
			return
		}
		log.Printf("tuta: test authorize failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
		writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱、密码和两步验证码", err)
		return
	}
	log.Printf("tuta: test authorize ok in %s", time.Since(start).Round(time.Millisecond))
	folders, err := listFoldersBounded(cl, listFoldersTimeout)
	if err != nil {
		log.Printf("tuta: test list folders failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
		writeUserErr(w, http.StatusBadGateway, "读取文件夹列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders), "capabilities": cl.Capabilities()})
}

const listFoldersTimeout = 45 * time.Second

func listFoldersBounded(cl mail.Conn, d time.Duration) ([]mail.FolderInfo, error) {
	type result struct {
		folders []mail.FolderInfo
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		folders, err := cl.ListFolders()
		ch <- result{folders, err}
	}()
	select {
	case r := <-ch:
		return r.folders, r.err
	case <-time.After(d):
		return nil, errors.New("list folders timed out")
	}
}

func (s *Server) testTutaStored(w http.ResponseWriter, key []byte, id int64) {
	start := time.Now()
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存的登录凭据不可用，请重新登录")
		return
	}
	cl, err := tuta.Connect(s.st, acct)
	if err != nil {
		log.Printf("tuta: test stored account %d connect failed after %s: %v", id, time.Since(start).Round(time.Millisecond), err)
		writeUserErr(w, http.StatusBadGateway, "连接失败，请检查服务器设置和账号密码", err)
		return
	}
	defer cl.Close()
	folders, err := listFoldersBounded(cl, listFoldersTimeout)
	if err != nil {
		log.Printf("tuta: test stored account %d list folders failed after %s: %v", id, time.Since(start).Round(time.Millisecond), err)
		writeUserErr(w, http.StatusBadGateway, "读取文件夹列表失败", err)
		return
	}
	log.Printf("tuta: test stored account %d ok in %s (%d folders)", id, time.Since(start).Round(time.Millisecond), len(folders))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folders": len(folders), "capabilities": cl.Capabilities()})
}

func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "账号编号不对")
		return
	}
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	var cl mail.Conn
	if acct.Kind == "proton" {
		cl, err = proton.Connect(s.st, acct)
	} else if acct.Kind == "tuta" {
		cl, err = tuta.Connect(s.st, acct)
	} else {
		cl, err = mail.Connect(acct, nil)
	}
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "连接失败，请检查服务器设置和账号密码", err)
		return
	}
	defer cl.Close()
	caps := cl.Capabilities()
	var folders []mail.FolderInfo
	if acct.Kind == "tuta" {
		folders, err = listFoldersBounded(cl, listFoldersTimeout)
	} else {
		folders, err = cl.ListFolders()
	}
	if err != nil {
		if acct.Kind == "tuta" {
			log.Printf("tuta: test account %d list folders failed: %v", id, err)
		}
		writeUserErr(w, http.StatusBadGateway, "读取文件夹列表失败", err)
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
		writeErr(w, http.StatusBadRequest, "账号编号不对")
		return
	}
	writeJSON(w, http.StatusOK, s.mgr.Status(id))
}

type reconnectIn struct {
	Password        string `json:"password"`
	MailboxPassword string `json:"mailbox_password"`
	TOTP            string `json:"totp"`
	CaptchaToken    string `json:"captcha_token"`
}

func (s *Server) handleReconnectAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "账号编号不对")
		return
	}
	key, ok := s.credentialKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "尚未登录")
		return
	}
	acct, err := s.loadMailAccount(id, key)
	if err != nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	var in reconnectIn
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &in); err != nil {
			writeErr(w, http.StatusBadRequest, "请求格式不对")
			return
		}
	}
	if acct.Kind == "proton" && in.Password != "" {
		ain := &accountIn{Email: acct.Email, Password: in.Password, MailboxPassword: in.MailboxPassword, TOTP: in.TOTP, CaptchaToken: in.CaptchaToken}
		bridgePassword, err := s.authorizeProton(ain)
		if err == proton.ErrTOTPRequired {
			writeJSON(w, http.StatusOK, map[string]bool{"needs_totp": true})
			return
		}
		var capErr *proton.CaptchaError
		if errors.As(err, &capErr) {
			writeJSON(w, http.StatusOK, map[string]any{"needs_captcha": true, "captcha_url": capErr.URL, "captcha_token": capErr.Token})
			return
		}
		if err != nil {
			writeUserErr(w, http.StatusBadRequest, "登录失败，请检查邮箱和密码", err)
			return
		}
		enc, err := auth.Encrypt(key, bridgePassword)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密失败")
			return
		}
		if _, err := s.st.DB.Exec(`UPDATE accounts SET enc_password = ? WHERE id = ?`, enc, id); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新失败")
			return
		}
		acct.Password = bridgePassword
	}
	s.mgr.Restart(acct)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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

func (s *Server) handlePurgeFolder(w http.ResponseWriter, r *http.Request) {
	folderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "文件夹编号不对")
		return
	}
	var accountID int64
	var name, special string
	if err := s.st.DB.QueryRow("SELECT account_id, name, COALESCE(special_use, '') FROM folders WHERE id = ?", folderID).Scan(&accountID, &name, &special); err != nil {
		writeErr(w, http.StatusNotFound, "文件夹不存在")
		return
	}
	if special != "trash" && special != "junk" {
		writeErr(w, http.StatusBadRequest, "只有回收站和垃圾箱可以清空")
		return
	}
	rows, err := s.st.DB.Query("SELECT id FROM messages WHERE folder_id = ?", folderID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	var ids []int64
	for rows.Next() {
		var mid int64
		if err := rows.Scan(&mid); err == nil {
			ids = append(ids, mid)
		}
	}
	rows.Close()
	deleted := 0
	for _, mid := range ids {
		if _, err := s.st.EnqueueExpunge(accountID, mid, ""); err == nil {
			deleted++
		}
	}
	if deleted > 0 {
		s.broadcastRemoved(folderID, ids)
		s.broadcastFolder(folderID)
		s.kickDrain()
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
}

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.DB.Query(`SELECT f.id, f.account_id, a.label, f.name, f.special_use, f.total, f.unread
		FROM folders f JOIN accounts a ON a.id = f.account_id
		ORDER BY f.account_id, f.id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "文件夹列表加载失败")
		return
	}
	defer rows.Close()
	out := []folderOut{}
	for rows.Next() {
		var f folderOut
		if err := rows.Scan(&f.ID, &f.AccountID, &f.AccountLabel, &f.Name, &f.SpecialUse, &f.Total, &f.Unread); err != nil {
			writeErr(w, http.StatusInternalServerError, "文件夹列表加载失败")
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
	TrashOriginFolderID int64 `json:"trash_origin_folder_id"`
}

type threadOut struct {
	messageOut
	ThreadCount  int `json:"thread_count"`
	ThreadUnread int `json:"thread_unread"`
}

const messageCols = `m.id, m.account_id, a.label, m.folder_id, f.name, f.special_use,
	m.thread_key, m.subject, m.from_json, m.date_ts, m.flags, m.has_attach, m.snippet, m.size,
	m.trash_origin_folder_id`

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
		&hasAttach, &mo.Snippet, &mo.Size, &mo.TrashOriginFolderID); err != nil {
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

type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Server) messageRowsByIDs(ids []int64) []messageOut {
	return s.messageRowsByIDsQ(s.st.DB, ids)
}

func (s *Server) messageRowsByIDsQ(q rowQuerier, ids []int64) []messageOut {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	dbRows, err := q.Query(`SELECT `+messageCols+` `+messageJoins+
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
	if get("thread_all") == "1" {
	} else if fid := get("folder_id"); fid != "" {
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
	where += " AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention'))"
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
	if q.Get("special") == "scheduled" {
		s.listScheduledAsMessages(w, q, limit, offset)
		return
	}
	where, args := listScope(q)

	if tk := q.Get("thread_key"); tk != "" {
		out := []messageOut{}
		if acctID, perr := strconv.ParseInt(q.Get("account_id"), 10, 64); perr == nil && acctID > 0 {
			ids, err := s.st.ThreadMembers(acctID, tk)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
				return
			}
			byID := make(map[int64]messageOut, len(ids))
			for _, mo := range s.messageRowsByIDs(ids) {
				byID[mo.ID] = mo
			}
			for _, id := range ids {
				if mo, ok := byID[id]; ok {
					out = append(out, mo)
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	tx, err := s.st.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	defer tx.Rollback()

	if q.Get("threaded") == "1" {
		type pageThread struct {
			acct   int64
			tk     string
			cnt    int
			unread int
			lastTS int64
			tieID  int64
			repID  int64
		}
		var threads []pageThread
		kRows, err := tx.Query(`SELECT m.account_id, m.thread_key,
				MAX(m.date_ts), MAX(m.id)
			FROM messages m JOIN folders f ON f.id = m.folder_id
			WHERE `+where+` AND m.thread_key <> ''
			GROUP BY m.account_id, m.thread_key
			ORDER BY MAX(m.date_ts) DESC, MAX(m.id) DESC`, args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
			return
		}
		for kRows.Next() {
			var t pageThread
			if err := kRows.Scan(&t.acct, &t.tk, &t.lastTS, &t.tieID); err != nil {
				kRows.Close()
				writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
				return
			}
			threads = append(threads, t)
		}
		kRows.Close()
		if err := kRows.Err(); err != nil {
			writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
			return
		}
		sRows, err := tx.Query(`SELECT m.id, m.account_id, m.date_ts,
				CASE WHEN (m.flags & 1) = 0 THEN 1 ELSE 0 END
			`+messageJoins+`
			WHERE `+where+` AND m.thread_key = ''
			ORDER BY m.date_ts DESC, m.id DESC`, args...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
			return
		}
		for sRows.Next() {
			var t pageThread
			if err := sRows.Scan(&t.repID, &t.acct, &t.lastTS, &t.unread); err != nil {
				sRows.Close()
				writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
				return
			}
			t.tk = "id:" + strconv.FormatInt(t.repID, 10)
			t.cnt = 1
			t.tieID = t.repID
			threads = append(threads, t)
		}
		sRows.Close()
		if err := sRows.Err(); err != nil {
			writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
			return
		}
		sort.Slice(threads, func(i, j int) bool {
			if threads[i].lastTS != threads[j].lastTS {
				return threads[i].lastTS > threads[j].lastTS
			}
			return threads[i].tieID > threads[j].tieID
		})
		if offset > len(threads) {
			offset = len(threads)
		}
		end := offset + limit
		if end > len(threads) {
			end = len(threads)
		}
		page := threads[offset:end]
		keysByAcct := make(map[int64][]string)
		for i := range page {
			if page[i].repID == 0 {
				keysByAcct[page[i].acct] = append(keysByAcct[page[i].acct], page[i].tk)
			}
		}
		for acct, keys := range keysByAcct {
			counts, err := s.st.ThreadGroupCountsQ(tx, acct, keys)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
				return
			}
			for i := range page {
				if page[i].acct == acct {
					if gc, ok := counts[page[i].tk]; ok {
						page[i].cnt = gc.Count
						page[i].unread = gc.Unread
					}
				}
			}
		}
		repIDs := make([]int64, 0, len(page))
		for i := range page {
			if page[i].repID == 0 {
				var id int64
				repArgs := append(append([]any{}, args...), page[i].acct, page[i].tk)
				if err := tx.QueryRow(`SELECT m.id
					FROM messages m JOIN folders f ON f.id = m.folder_id
					WHERE `+where+` AND m.account_id = ? AND m.thread_key = ?
					ORDER BY m.date_ts DESC, m.id DESC LIMIT 1`, repArgs...).Scan(&id); err != nil {
					writeErr(w, http.StatusInternalServerError, "会话列表加载失败")
					return
				}
				page[i].repID = id
			}
			repIDs = append(repIDs, page[i].repID)
		}
		byID := make(map[int64]messageOut, len(repIDs))
		for _, mo := range s.messageRowsByIDsQ(tx, repIDs) {
			byID[mo.ID] = mo
		}
		out := []threadOut{}
		for _, t := range page {
			mo, ok := byID[t.repID]
			if !ok {
				continue
			}
			to := threadOut{messageOut: mo, ThreadCount: t.cnt, ThreadUnread: t.unread}
			if to.ThreadKey == "" {
				to.ThreadKey = "id:" + strconv.FormatInt(to.ID, 10)
			}
			out = append(out, to)
		}
		wm, err := listVersionTx(tx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
			return
		}
		if err := tx.Commit(); err != nil {
			writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
			return
		}
		w.Header().Set("X-WM-Ver", strconv.FormatInt(wm, 10))
		writeJSON(w, http.StatusOK, out)
		return
	}

	dbRows, err := tx.Query(`SELECT `+messageCols+` `+messageJoins+
		` WHERE `+where+` ORDER BY m.date_ts DESC, m.id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	out := []messageOut{}
	for dbRows.Next() {
		var mo messageOut
		if err := scanMessage(dbRows, &mo); err != nil {
			dbRows.Close()
			writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
			return
		}
		out = append(out, mo)
	}
	dbRows.Close()
	if err := dbRows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	wm, err := listVersionTx(tx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "邮件列表加载失败")
		return
	}
	w.Header().Set("X-WM-Ver", strconv.FormatInt(wm, 10))
	writeJSON(w, http.StatusOK, out)
}

func listVersionTx(tx *sql.Tx) (int64, error) {
	var v int64
	err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM place_log`).Scan(&v)
	return v, err
}

type turnConn struct {
	mail.Conn
	end func()
}

func (c *turnConn) Close() {
	c.Conn.Close()
	if c.end != nil {
		c.end()
		c.end = nil
	}
}

func (s *Server) openConn(key []byte, accountID int64) (mail.Conn, error) {
	acct, err := s.loadMailAccount(accountID, key)
	if err != nil {
		return nil, err
	}
	if acct.Kind == "proton" {
		sess, err := proton.Connect(s.st, acct)
		if err != nil {
			return nil, err
		}
		return &turnConn{Conn: sess, end: sess.UserTurn()}, nil
	}
	if acct.Kind == "tuta" {
		sess, err := tuta.Connect(s.st, acct)
		if err != nil {
			return nil, err
		}
		return &turnConn{Conn: sess, end: sess.UserTurn()}, nil
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
	BodyTruncated bool                 `json:"body_truncated,omitempty"`
	Attachments  []store.AttachmentRow `json:"attachments"`
	ThreadKey    string                `json:"thread_key"`
	FolderSpecial string               `json:"folder_special"`
	ThreadCount  int                   `json:"thread_count"`
}

func parseAddrs(jsonStr string) []mail.Addr {
	var addrs []mail.Addr
	if err := json.Unmarshal([]byte(jsonStr), &addrs); err != nil || addrs == nil {
		return []mail.Addr{}
	}
	return addrs
}

var errFolderEpochChanged = errors.New("文件夹已在服务器上重建，正在重新同步")

func (s *Server) ensureBody(key []byte, messageID int64, route store.MessageRoute) (string, string, []store.AttachmentRow, bool, error) {
	if route.BodyState == 1 || route.BodyState == 2 {
		text, html, _, err := s.st.GetBody(messageID)
		if err != nil {
			return "", "", nil, false, err
		}
		atts, err := s.st.ListAttachments(messageID)
		return text, html, atts, route.BodyState == 2, err
	}
	cl, err := s.openConn(key, route.AccountID)
	if err != nil {
		return "", "", nil, false, err
	}
	defer cl.Close()
	sel, err := cl.Select(route.Folder)
	if err != nil {
		return "", "", nil, false, err
	}
	if route.FolderUIDValidity != 0 && sel.UIDValidity != 0 && sel.UIDValidity != route.FolderUIDValidity {
		return "", "", nil, false, errFolderEpochChanged
	}
	raw, err := cl.FetchRaw(route.UID)
	if err != nil {
		return "", "", nil, false, err
	}
	parsed, err := mail.ParseBody(raw)
	if err != nil {
		return "", "", nil, false, err
	}
	if err := s.st.SaveBody(messageID, parsed.Text, parsed.HTML, mail.MakeSnippet(parsed.Text, parsed.HTML), parsed.Truncated); err != nil {
		return "", "", nil, false, err
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
		return "", "", nil, false, err
	}
	return parsed.Text, parsed.HTML, atts, parsed.Truncated, nil
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
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
		m.from_json, m.to_json, m.cc_json, m.date_ts, m.flags, m.size, m.has_attach, m.thread_key, f.special_use
		FROM messages m JOIN accounts a ON a.id = m.account_id JOIN folders f ON f.id = m.folder_id
		WHERE m.id = ?`, id).
		Scan(&d.AccountID, &d.AccountLabel, &d.FolderID, &d.FolderName, &d.Subject,
			&fromJSON, &toJSON, &ccJSON, &d.DateTS, &d.Flags, &d.Size, &hasAttach, &d.ThreadKey, &d.FolderSpecial)
	if err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	d.ID = id
	d.HasAttach = hasAttach != 0
	if d.ThreadKey != "" {
		d.ThreadCount, _ = s.st.ThreadUniqueCount(d.AccountID, d.ThreadKey)
	}
	from := parseAddrs(fromJSON)
	if len(from) > 0 {
		d.From = &from[0]
	}
	d.To = parseAddrs(toJSON)
	d.Cc = parseAddrs(ccJSON)

	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "定位邮件失败")
		return
	}
	text, html, atts, truncated, err := s.ensureBody(key, id, route)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "正文加载失败，请稍后重试", err)
		return
	}
	d.Text = text
	d.BodyTruncated = truncated
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
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	attID, err := strconv.ParseInt(r.PathValue("attId"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "附件编号不对")
		return
	}
	att, err := s.st.GetAttachment(id, attID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "附件不存在")
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
		writeErr(w, http.StatusInternalServerError, "定位邮件失败")
		return
	}
	cl, err := s.openConn(key, route.AccountID)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "邮件不存在或已被删除", err)
		return
	}
	defer cl.Close()
	if _, err := cl.Select(route.Folder); err != nil {
		writeUserErr(w, http.StatusBadGateway, "连接邮件服务器失败", err)
		return
	}
	raw, err := cl.FetchRaw(route.UID)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "打开文件夹失败", err)
		return
	}
	part, err := mail.ParseBodyPart(raw, att.PartPath)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "读取邮件内容失败", err)
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
	Seen      *bool  `json:"seen"`
	Flagged   *bool  `json:"flagged"`
	ClientKey string `json:"client_key"`
}

func (s *Server) handleSetFlags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	var req flagsReq
	if err := decodeJSON(r, &req); err != nil || (req.Seen == nil && req.Flagged == nil) {
		writeErr(w, http.StatusBadRequest, "缺少标记参数")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	opID, flags, err := s.st.EnqueueFlags(route.AccountID, id, req.Seen, req.Flagged, req.ClientKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "操作入队失败")
		return
	}
	s.broadcastFlags([]int64{id})
	s.broadcastFolder(route.FolderID)
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags, "op_id": opID})
}

type batchFlagsReq struct {
	IDs       []int64 `json:"ids"`
	Seen      *bool   `json:"seen"`
	Flagged   *bool   `json:"flagged"`
	ClientKey string  `json:"client_key"`
}

type batchItemOut struct {
	ID    int64  `json:"id"`
	Flags uint32 `json:"flags"`
	OpID  int64  `json:"op_id"`
}

type batchFailOut struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

func (s *Server) handleBatchFlags(w http.ResponseWriter, r *http.Request) {
	var req batchFlagsReq
	if err := decodeJSON(r, &req); err != nil || len(req.IDs) == 0 || (req.Seen == nil && req.Flagged == nil) {
		writeErr(w, http.StatusBadRequest, "缺少标记参数")
		return
	}
	items := []batchItemOut{}
	failed := []batchFailOut{}
	folders := map[int64]bool{}
	ids := []int64{}
	for _, id := range req.IDs {
		route, err := s.st.MessageRoute(id)
		if err != nil {
			failed = append(failed, batchFailOut{ID: id, Error: "邮件不存在"})
			continue
		}
		key := ""
		if req.ClientKey != "" {
			key = req.ClientKey + ":" + strconv.FormatInt(id, 10)
		}
		opID, flags, err := s.st.EnqueueFlags(route.AccountID, id, req.Seen, req.Flagged, key)
		if err != nil {
			failed = append(failed, batchFailOut{ID: id, Error: "操作入队失败"})
			continue
		}
		items = append(items, batchItemOut{ID: id, Flags: flags, OpID: opID})
		ids = append(ids, id)
		folders[route.FolderID] = true
	}
	if len(ids) > 0 {
		s.broadcastFlags(ids)
		for fid := range folders {
			s.broadcastFolder(fid)
		}
		s.kickDrain()
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "failed": failed})
}

func isNotInFolder(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found in")
}

type moveReq struct {
	To         string `json:"to"`
	ToFolderID int64  `json:"to_folder_id"`
	ClientKey  string `json:"client_key"`
}

type deleteReq struct {
	Permanent *bool  `json:"permanent"`
	ClientKey string `json:"client_key"`
}

func (s *Server) findMovedUID(cl mail.Conn, id int64, destFolder string) uint32 {
	var mid string
	if err := s.st.DB.QueryRow("SELECT message_id FROM messages WHERE id = ?", id).Scan(&mid); err != nil || mid == "" {
		return 0
	}
	finder, ok := cl.(interface {
		FindUIDByMessageID(string, string) (uint32, error)
	})
	if !ok {
		return 0
	}
	uid, err := finder.FindUIDByMessageID(destFolder, mid)
	if err != nil {
		return 0
	}
	return uid
}

func (s *Server) handleMoveMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	var req moveReq
	if err := decodeJSON(r, &req); err != nil || (req.ToFolderID == 0 && req.To == "") {
		writeErr(w, http.StatusBadRequest, "请选择目标文件夹")
		return
	}
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	destID := req.ToFolderID
	if destID == 0 {
		var ok bool
		destID, _, ok, err = s.st.FolderBySpecial(route.AccountID, req.To)
		if err != nil || !ok {
			writeErr(w, http.StatusBadRequest, "目标文件夹不存在")
			return
		}
	}
	if destID == route.FolderID {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	opID, err := s.st.EnqueueMove(route.AccountID, id, destID, req.ClientKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "操作入队失败")
		return
	}
	o, _ := s.st.GetOp(opID)
	if o != nil {
		s.broadcastOpApplied(o)
	}
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID})
}

func (s *Server) handleRestoreMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	var req moveReq
	_ = decodeJSON(r, &req)
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	destID, err := s.st.ResolveRestoreFolder(route.AccountID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "确定还原位置失败")
		return
	}
	if destID == route.FolderID {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to_folder_id": destID})
		return
	}
	opID, err := s.st.EnqueueMove(route.AccountID, id, destID, req.ClientKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "操作入队失败")
		return
	}
	o, _ := s.st.GetOp(opID)
	if o != nil {
		s.broadcastOpApplied(o)
	}
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID, "to_folder_id": destID})
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "邮件编号不对")
		return
	}
	var req deleteReq
	_ = decodeJSON(r, &req)
	permanent := req.Permanent != nil && *req.Permanent
	route, err := s.st.MessageRoute(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "邮件不存在")
		return
	}
	var special string
	_ = s.st.DB.QueryRow("SELECT COALESCE(special_use, '') FROM folders WHERE id = ?", route.FolderID).Scan(&special)
	var opID int64
	if req.ClientKey != "" {
		if prev, perr := s.st.OpByClientKey(req.ClientKey); perr == nil && prev != nil &&
			prev.AccountID == route.AccountID && prev.MessageID == id &&
			(prev.Type == store.OpMove || prev.Type == store.OpExpunge) {
			s.kickDrain()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": prev.ID})
			return
		}
	}
	if permanent || special == "trash" || special == "junk" {
		opID, err = s.st.EnqueueExpunge(route.AccountID, id, req.ClientKey)
	} else {
		var trashID int64
		var ok bool
		trashID, _, ok, err = s.st.FolderBySpecial(route.AccountID, "trash")
		if err == nil && ok {
			opID, err = s.st.EnqueueMove(route.AccountID, id, trashID, req.ClientKey)
		} else if err == nil {
			err = errors.New("no trash folder")
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "操作入队失败")
		return
	}
	o, _ := s.st.GetOp(opID)
	if o != nil {
		s.broadcastOpApplied(o)
	}
	s.kickDrain()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op_id": opID})
}

type markAllReadReq struct {
	Special  string `json:"special"`
	FolderID int64  `json:"folder_id"`
}

func (s *Server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	var req markAllReadReq
	if err := decodeJSON(r, &req); err != nil || (req.FolderID == 0 && req.Special == "") {
		writeErr(w, http.StatusBadRequest, "缺少文件夹参数")
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
			writeErr(w, http.StatusNotFound, "文件夹不存在")
			return
		}
		refs = append(refs, fr)
	} else {
		rows, err := s.st.DB.Query("SELECT id, account_id, name FROM folders WHERE special_use = ?", req.Special)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查找文件夹失败")
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
		rows, err := s.st.DB.Query(`SELECT id FROM messages m
			WHERE m.folder_id = ? AND (m.flags & 1) = 0
			AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = m.id AND o.type = 'expunge'
				AND o.status IN ('pending', 'in_flight', 'needs_attention'))`, fr.id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查找邮件失败")
			return
		}
		var ids []int64
		for rows.Next() {
			var mid int64
			if err := rows.Scan(&mid); err == nil {
				ids = append(ids, mid)
			}
		}
		rows.Close()
		seenTrue := true
		doneIDs := []int64{}
		for _, mid := range ids {
			_, flags, err := s.st.EnqueueFlags(fr.accountID, mid, &seenTrue, nil, "")
			if err != nil {
				continue
			}
			if flags&1 != 0 {
				updated++
			}
			doneIDs = append(doneIDs, mid)
		}
		if len(doneIDs) > 0 {
			s.broadcastFlags(doneIDs)
			s.broadcastFolder(fr.id)
		}
	}
	if updated > 0 {
		s.kickDrain()
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
		return nil, userFacing("收件人格式不对")
	}
	cc, err := parseAddrList(req.Cc)
	if err != nil {
		return nil, userFacing("抄送格式不对")
	}
	bcc, err := parseAddrList(req.Bcc)
	if err != nil {
		return nil, userFacing("密送格式不对")
	}
	rcpts := []string{}
	for _, group := range [][]mail.Addr{to, cc, bcc} {
		for _, a := range group {
			rcpts = append(rcpts, a.Email)
		}
	}
	if requireRcpts && len(rcpts) == 0 {
		return nil, userFacing("至少填写一个收件人")
	}
	from := mail.Addr{Name: acct.FromName, Email: acct.Email}

	attachments := make([]mail.OutgoingAttachment, 0, len(req.Attachments))
	totalAttach := 0
	for _, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataB64)
		if err != nil {
			return nil, userFacing(fmt.Sprintf("附件 %s 的数据无效", a.Filename))
		}
		totalAttach += len(data)
		if int64(totalAttach) > s.st.AppSettings().MaxAttachmentMB<<20 {
			return nil, userFacing(fmt.Sprintf("附件总大小超过 %d MB 上限", s.st.AppSettings().MaxAttachmentMB))
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
	var existing int64
	if err := s.st.DB.QueryRow("SELECT id FROM messages WHERE folder_id = ? AND message_id = ?",
		folderID, out.messageID).Scan(&existing); err == nil {
		return existing, nil
	}
	cl, err := s.appendOpen(key, accountID)
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
		ThreadKey: s.st.ComputeThreadKey(accountID, out.messageID, out.inReplyTo, out.subject),
		Subject:   out.subject,
		FromJSON:  mustJSON([]mail.Addr{out.from}),
		ToJSON:    mustJSON(out.to),
		CcJSON:    mustJSON(out.cc),
		DateTS:    time.Now().Unix(),
		Flags:     localFlags,
		Size:      int64(len(out.raw)),
	}
	id, inserted, err := s.st.UpsertMessage(accountID, folderID, row)
	if err != nil {
		return 0, err
	}
	if err := s.st.SaveBody(id, out.text, out.html, mail.MakeSnippet(out.text, out.html), false); err != nil {
		return 0, err
	}
	if parsed, perr := mail.ParseBody(out.raw); perr == nil && len(parsed.Attachments) > 0 {
		attRows := make([]store.AttachmentRow, 0, len(parsed.Attachments))
		for _, a := range parsed.Attachments {
			attRows = append(attRows, store.AttachmentRow{
				PartPath: a.PartPath, Filename: a.Filename, Mime: a.Mime,
				Size: a.Size, ContentID: a.ContentID, Inline: a.Inline,
			})
		}
		if _, err := s.st.ReplaceAttachments(id, attRows); err == nil {
			_, _ = s.st.DB.Exec("UPDATE messages SET has_attach = 1 WHERE id = ?", id)
		}
	}
	if inserted {
		_, _ = s.st.DB.Exec("UPDATE folders SET total = total + 1 WHERE id = ?", folderID)
	}
	_ = s.st.RefreshUnread(folderID)
	return id, nil
}

type uidAssigner interface {
	AssignUID(folder, id string) (uint32, error)
}

func (s *Server) fileSentCopy(acct mail.Account, sess uidAssigner, sentID string, out *outgoing) (int64, error) {
	folderID, folderName, ok, err := s.st.FolderBySpecial(acct.ID, "sent")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("该账号没有 %s 文件夹", "sent")
	}
	uid, err := sess.AssignUID(folderName, sentID)
	if err != nil {
		return 0, err
	}
	row := &store.MessageRow{
		UID:       uid,
		MessageID: out.messageID,
		ThreadKey: s.st.ComputeThreadKey(acct.ID, out.messageID, out.inReplyTo, out.subject),
		Subject:   out.subject,
		FromJSON:  mustJSON([]mail.Addr{out.from}),
		ToJSON:    mustJSON(out.to),
		CcJSON:    mustJSON(out.cc),
		DateTS:    time.Now().Unix(),
		Flags:     mail.FlagSeen,
		Size:      int64(len(out.raw)),
	}
	id, inserted, err := s.st.UpsertMessage(acct.ID, folderID, row)
	if err != nil {
		return 0, err
	}
	if err := s.st.SaveBody(id, out.text, out.html, mail.MakeSnippet(out.text, out.html), false); err != nil {
		return 0, err
	}
	if inserted {
		_, _ = s.st.DB.Exec("UPDATE folders SET total = total + 1 WHERE id = ?", folderID)
	}
	_ = s.st.RefreshUnread(folderID)
	return id, nil
}

func (s *Server) deleteMessageEverywhere(key []byte, messageID int64) error {
	route, err := s.st.MessageRoute(messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cl, err := s.openConn(key, route.AccountID)
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
	if err := s.st.DeleteLocalMessage(messageID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}


func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	key, _ := s.credentialKey(r)
	var req composeReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	out, err := s.prepareOutgoing(key, req, false)
	if err != nil {
		writeUserErr(w, http.StatusBadRequest, "邮件准备失败，请检查内容后重试", err)
		return
	}
	id, err := s.appendCopy(key, req.AccountID, "drafts",
		[]imap.Flag{imap.FlagDraft}, mail.FlagSeen|mail.FlagDraft, out)
	if err != nil {
		writeUserErr(w, http.StatusBadGateway, "保存草稿失败", err)
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
		writeJSON(w, http.StatusOK, []threadOut{})
		return
	}
	limit := 60
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	const pickedWindow = `SELECT mm.id, ROW_NUMBER() OVER (PARTITION BY mm.account_id,
		CASE WHEN mm.message_id = '' THEN 'id:' || mm.id ELSE mm.message_id END, mm.date_ts
		ORDER BY CASE WHEN ff.special_use = 'all' THEN 1 ELSE 0 END, mm.id) AS rn FROM `
	const pickedTail = ` AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = mm.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention'))) picked
		JOIN messages m ON m.id = picked.id
		JOIN accounts a ON a.id = m.account_id
		JOIN folders f ON f.id = m.folder_id
		WHERE picked.rn = 1 ORDER BY m.date_ts DESC, m.id DESC LIMIT ? OFFSET ?`
	const searchCols = `SELECT m.id, m.account_id, a.label, m.folder_id, f.name, f.special_use, m.thread_key, m.subject,
		m.from_json, m.date_ts, m.flags, m.has_attach, m.snippet, m.size,
		(SELECT COUNT(*) FROM (SELECT 1 FROM messages t WHERE t.account_id = m.account_id AND t.thread_key = m.thread_key AND m.thread_key <> '' AND NOT EXISTS (SELECT 1 FROM ops o WHERE o.message_id = t.id AND o.type = 'expunge' AND o.status IN ('pending', 'in_flight', 'needs_attention')) GROUP BY CASE WHEN t.message_id = '' THEN 'id:' || t.id ELSE t.message_id END, t.date_ts))
		FROM (`
	terms := strings.Fields(q)
	short := len(terms) > 0
	for _, t := range terms {
		if len([]rune(t)) >= 3 {
			short = false
		}
	}
	var rows *sql.Rows
	var err error
	if short {
		conds := make([]string, 0, len(terms))
		args := make([]any, 0, len(terms)*4+1)
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
		for _, t := range terms {
			like := "%" + esc.Replace(t) + "%"
			conds = append(conds, `(mm.subject LIKE ? ESCAPE '\' OR mm.from_json LIKE ? ESCAPE '\' OR mm.to_json LIKE ? ESCAPE '\' OR mm.cc_json LIKE ? ESCAPE '\')`)
			args = append(args, like, like, like, like)
		}
		args = append(args, limit, offset)
		rows, err = s.st.DB.Query(searchCols+pickedWindow+`messages mm
			JOIN folders ff ON ff.id = mm.folder_id
			WHERE `+strings.Join(conds, " AND ")+pickedTail, args...)
	} else {
		rows, err = s.st.DB.Query(searchCols+pickedWindow+`messages_fts
			JOIN messages mm ON mm.id = messages_fts.rowid
			JOIN folders ff ON ff.id = mm.folder_id
			WHERE messages_fts MATCH ?`+pickedTail,
			store.BuildFTSQuery(q), limit, offset)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "搜索失败")
		return
	}
	defer rows.Close()
	out := []threadOut{}
	for rows.Next() {
		var row messageOut
		var fromJSON string
		var hasAttach int
		var cnt int
		if err := rows.Scan(&row.ID, &row.AccountID, &row.AccountLabel, &row.FolderID, &row.FolderName, &row.FolderSpecial, &row.ThreadKey,
			&row.Subject, &fromJSON, &row.DateTS, &row.Flags, &hasAttach, &row.Snippet, &row.Size, &cnt); err != nil {
			writeErr(w, http.StatusInternalServerError, "读取数据失败")
			return
		}
		row.HasAttach = hasAttach != 0
		if from := parseAddrs(fromJSON); len(from) > 0 {
			row.FromName = from[0].Name
			row.FromEmail = from[0].Email
		}
		out = append(out, threadOut{messageOut: row, ThreadCount: cnt})
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
		writeErr(w, http.StatusInternalServerError, "读取通讯录失败")
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
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/m/") {
			log.Printf("entry: %s %s ua=%q referer=%q", r.Method, r.URL.Path, r.UserAgent(), r.Referer())
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "接口不存在")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
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
		writeErr(w, http.StatusInternalServerError, "读取缓存用量失败")
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
		writeErr(w, http.StatusBadRequest, "设置内容不对")
		return
	}
	if req.CacheBudgetGB < 0 || req.CacheBudgetGB > 1024 ||
		req.SyncBatch < 1 || req.SyncBatch > 1000 ||
		req.MaxAttachmentMB < 1 || req.MaxAttachmentMB > 200 ||
		req.PushWindowMinutes < 0 || req.PushWindowMinutes > 43200 {
		writeErr(w, http.StatusBadRequest, "设置超出允许范围")
		return
	}
	if err := s.st.SaveAppSettings(req); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
