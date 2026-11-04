package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"namakani/internal/auth"
	"namakani/internal/gmail"
	"namakani/internal/mail"
)

const gmailAuthTTL = 10 * time.Minute

type gmailAuthEntry struct {
	label        string
	email        string
	fromName     string
	signature    string
	clientID     string
	clientSecret string
	verifier     string
	redirectURI  string
	accountID    int64
	keyFP        string
	at           time.Time
}

type gmailStartIn struct {
	Label     string `json:"label"`
	Email     string `json:"email"`
	FromName  string `json:"from_name"`
	Signature string `json:"signature"`
	AccountID int64  `json:"account_id"`
}

type gmailPendingEntry struct {
	label        string
	email        string
	fromName     string
	signature    string
	enc          string
	folders      int
	capabilities []string
	keyFP        string
	at           time.Time
}

type gmailReauthEntry struct {
	accountID    int64
	label        string
	email        string
	fromName     string
	signature    string
	folders      int
	capabilities []string
	keyFP        string
	at           time.Time
}

func keyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

func (s *Server) pruneGmailAuthLocked() {
	for state, e := range s.gmailAuth {
		if time.Since(e.at) > gmailAuthTTL {
			delete(s.gmailAuth, state)
		}
	}
}

func (s *Server) pruneGmailPendingLocked() {
	for id, e := range s.gmailPending {
		if time.Since(e.at) > gmailAuthTTL {
			delete(s.gmailPending, id)
		}
	}
}

func (s *Server) gmailPendingFor(id string, key []byte, consume bool) (gmailPendingEntry, bool) {
	if id == "" {
		return gmailPendingEntry{}, false
	}
	s.gmailPendingMu.Lock()
	defer s.gmailPendingMu.Unlock()
	s.pruneGmailPendingLocked()
	e, ok := s.gmailPending[id]
	if !ok || e.keyFP != keyFingerprint(key) {
		return gmailPendingEntry{}, false
	}
	if consume {
		delete(s.gmailPending, id)
	}
	return e, true
}

func (s *Server) handleGetGmailPending(w http.ResponseWriter, r *http.Request) {
	key, ok := s.credentialKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")
	e, ok := s.gmailPendingFor(id, key, false)
	if !ok {
		writeErr(w, http.StatusNotFound, "authorization expired, start the authorization again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending_id":   id,
		"label":        e.label,
		"email":        e.email,
		"from_name":    e.fromName,
		"signature":    e.signature,
		"folders":      e.folders,
		"capabilities": e.capabilities,
	})
}

func (s *Server) handleDeleteGmailPending(w http.ResponseWriter, r *http.Request) {
	key, ok := s.credentialKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	if _, ok := s.gmailPendingFor(r.PathValue("id"), key, true); !ok {
		writeErr(w, http.StatusNotFound, "authorization expired, start the authorization again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) pruneGmailReauthLocked() {
	for id, e := range s.gmailReauth {
		if time.Since(e.at) > gmailAuthTTL {
			delete(s.gmailReauth, id)
		}
	}
}

func (s *Server) handleGetGmailReauth(w http.ResponseWriter, r *http.Request) {
	key, ok := s.credentialKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	id := r.PathValue("id")
	s.gmailReauthMu.Lock()
	s.pruneGmailReauthLocked()
	e, ok := s.gmailReauth[id]
	s.gmailReauthMu.Unlock()
	if !ok || e.keyFP != keyFingerprint(key) {
		writeErr(w, http.StatusNotFound, "authorization expired, start the authorization again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id":   e.accountID,
		"label":        e.label,
		"email":        e.email,
		"from_name":    e.fromName,
		"signature":    e.signature,
		"folders":      e.folders,
		"capabilities": e.capabilities,
	})
}

func (s *Server) createGmailAccount(w http.ResponseWriter, key []byte, in *accountIn) {
	e, ok := s.gmailPendingFor(in.GmailPendingID, key, true)
	if !ok {
		writeErr(w, http.StatusBadRequest, "use test connection first")
		return
	}
	res, err := s.st.DB.Exec(`INSERT INTO accounts(label, email, imap_host, imap_port, imap_tls,
		smtp_host, smtp_port, smtp_tls, username, enc_password, signature, from_name, tls_skip_verify, kind, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.label, e.email, "", 0, "none",
		"", 0, "none", e.email, e.enc, e.signature, e.fromName, false, "gmail", time.Now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the account")
		return
	}
	id, _ := res.LastInsertId()
	s.startWorkerFor(id, key)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleGmailStart(w http.ResponseWriter, r *http.Request) {
	key, ok := s.credentialKey(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not signed in")
		return
	}
	var in gmailStartIn
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	entry := gmailAuthEntry{accountID: in.AccountID}
	if in.AccountID > 0 {
		acct, err := s.loadMailAccount(in.AccountID, key)
		if err != nil || acct.Kind != "gmail" {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
		entry.label = acct.Label
		entry.email = acct.Email
	} else {
		entry.label = strings.TrimSpace(in.Label)
		entry.email = strings.TrimSpace(in.Email)
		entry.fromName = in.FromName
		entry.signature = in.Signature
		if entry.label == "" || !strings.Contains(entry.email, "@") {
			writeErr(w, http.StatusBadRequest, "name and email are required")
			return
		}
	}
	state, err := gmail.RandomState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to generate the authorization state")
		return
	}
	verifier, challenge, err := gmail.PKCEPair()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to generate the authorization parameters")
		return
	}
	base := s.st.BaseURL()
	if base == "" {
		writeErr(w, http.StatusBadRequest, "Base URL is not set, fill it in under settings first")
		return
	}
	shared := s.gmailOAuthLoad()
	if shared.ClientID == "" || shared.ClientSecret == "" {
		writeErr(w, http.StatusBadRequest, "google oauth client is not configured, fill it in under accounts and authentication in settings first")
		return
	}
	entry.clientID = shared.ClientID
	entry.clientSecret = shared.ClientSecret
	entry.verifier = verifier
	entry.redirectURI = base + "/api/gmail/callback"
	entry.keyFP = keyFingerprint(key)
	entry.at = time.Now()
	s.gmailAuthMu.Lock()
	s.pruneGmailAuthLocked()
	s.gmailAuth[state] = entry
	s.gmailAuthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"auth_url": gmail.AuthCodeURL(entry.clientID, entry.redirectURI, state, challenge, entry.email),
	})
}

func (s *Server) handleGmailPubSub(w http.ResponseWriter, r *http.Request) {
	cfg := s.gmailOAuthLoad()
	token := r.URL.Query().Get("token")
	if cfg.PubSubToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cfg.PubSubToken)) != 1 {
		writeErr(w, http.StatusForbidden, "invalid token")
		return
	}
	defer r.Body.Close()
	var in struct {
		Message struct {
			Data        string            `json:"data"`
			MessageID   string            `json:"messageId"`
			PublishTime string            `json:"publishTime"`
			Attributes  map[string]string `json:"attributes"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		log.Printf("gmail pubsub: undecodable request: %v", err)
		writeErr(w, http.StatusBadRequest, "undecodable request")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(in.Message.Data)
	if err != nil {
		log.Printf("gmail pubsub: undecodable message data: %v", err)
		writeErr(w, http.StatusBadRequest, "undecodable message data")
		return
	}
	var note struct {
		EmailAddress string `json:"emailAddress"`
	}
	if err := json.Unmarshal(raw, &note); err != nil || note.EmailAddress == "" {
		log.Printf("gmail pubsub: undecodable notification: %v", err)
		writeErr(w, http.StatusBadRequest, "undecodable notification")
		return
	}
	s.mgr.NotifyGmailPush(note.EmailAddress)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) updateGmailAccount(w http.ResponseWriter, r *http.Request, in *accountIn) {
	id := r.PathValue("id")
	if strings.TrimSpace(in.Label) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if _, err := s.st.DB.Exec(`UPDATE accounts SET label = ?, signature = ?, from_name = ? WHERE id = ? AND kind = 'gmail'`,
		in.Label, in.Signature, in.FromName, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func gmailAuthFail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}

func (s *Server) handleGmailCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("error") != "" {
		gmailAuthFail(w, http.StatusBadRequest, "google authorization was not completed: "+q.Get("error")+". go back to settings and start the authorization again")
		return
	}
	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		gmailAuthFail(w, http.StatusBadRequest, "authorization callback parameters are incomplete, go back to settings and start the authorization again")
		return
	}
	s.gmailAuthMu.Lock()
	entry, ok := s.gmailAuth[state]
	delete(s.gmailAuth, state)
	s.gmailAuthMu.Unlock()
	if !ok || time.Since(entry.at) > gmailAuthTTL {
		gmailAuthFail(w, http.StatusBadRequest, "authorization expired, go back to settings and start the authorization again")
		return
	}
	key, kok := s.credentialKey(r)
	if !kok {
		gmailAuthFail(w, http.StatusUnauthorized, "your session has expired, sign in again and then start the authorization")
		return
	}
	if keyFingerprint(key) != entry.keyFP {
		gmailAuthFail(w, http.StatusForbidden, "the authorization session does not match the current sign-in, start the authorization again")
		return
	}
	ctx := r.Context()
	creds, err := gmail.ExchangeCode(ctx, entry.clientID, entry.clientSecret, code, entry.verifier, entry.redirectURI)
	if err != nil {
		log.Printf("gmail: callback exchange code failed: %v", err)
		gmailAuthFail(w, http.StatusBadGateway, "failed to exchange the token with google, go back to settings and start the authorization again")
		return
	}
	profile, err := gmail.NewClient(creds).Profile(ctx)
	if err != nil || profile.EmailAddress == "" {
		log.Printf("gmail: callback profile failed: %v", err)
		gmailAuthFail(w, http.StatusBadGateway, "failed to read the google account profile, go back to settings and start the authorization again")
		return
	}
	enc, err := auth.Encrypt(key, gmail.TokenCredsJSON(creds.RefreshToken))
	if err != nil {
		gmailAuthFail(w, http.StatusInternalServerError, "failed to encrypt the credentials")
		return
	}
	acct := mail.Account{Kind: "gmail", Email: profile.EmailAddress, Username: profile.EmailAddress, Password: creds.JSON()}
	cl, err := gmail.Connect(s.st, acct)
	if err != nil {
		log.Printf("gmail: callback connect failed: %v", err)
		gmailAuthFail(w, http.StatusBadGateway, "connection failed, go back to settings and start the authorization again")
		return
	}
	folders, err := cl.ListFolders()
	capabilities := cl.Capabilities()
	cl.Close()
	if err != nil {
		log.Printf("gmail: callback list folders failed: %v", err)
		gmailAuthFail(w, http.StatusBadGateway, "failed to read the folder list, go back to settings and start the authorization again")
		return
	}
	if entry.accountID > 0 {
		var label, email, fromName, signature string
		if err := s.st.DB.QueryRow(`SELECT label, email, from_name, signature FROM accounts WHERE id = ? AND kind = 'gmail'`, entry.accountID).Scan(&label, &email, &fromName, &signature); err != nil {
			gmailAuthFail(w, http.StatusInternalServerError, "failed to read the account")
			return
		}
		if _, err := s.st.DB.Exec(`UPDATE accounts SET enc_password = ? WHERE id = ? AND kind = 'gmail'`, enc, entry.accountID); err != nil {
			gmailAuthFail(w, http.StatusInternalServerError, "failed to save the credentials")
			return
		}
		if acct, err := s.loadMailAccount(entry.accountID, key); err == nil {
			s.mgr.Restart(acct)
		}
		reauthID, err := gmail.RandomState()
		if err != nil {
			gmailAuthFail(w, http.StatusInternalServerError, "failed to generate the authorization state")
			return
		}
		s.gmailReauthMu.Lock()
		if s.gmailReauth == nil {
			s.gmailReauth = make(map[string]gmailReauthEntry)
		}
		s.pruneGmailReauthLocked()
		s.gmailReauth[reauthID] = gmailReauthEntry{
			accountID:    entry.accountID,
			label:        label,
			email:        email,
			fromName:     fromName,
			signature:    signature,
			folders:      len(folders),
			capabilities: capabilities,
			keyFP:        keyFingerprint(key),
			at:           time.Now(),
		}
		s.gmailReauthMu.Unlock()
		http.Redirect(w, r, "/?gmail_reauthed="+url.QueryEscape(reauthID)+"#/settings", http.StatusFound)
		return
	}
	pendingID, err := gmail.RandomState()
	if err != nil {
		gmailAuthFail(w, http.StatusInternalServerError, "failed to generate the authorization state")
		return
	}
	s.gmailPendingMu.Lock()
	if s.gmailPending == nil {
		s.gmailPending = make(map[string]gmailPendingEntry)
	}
	s.pruneGmailPendingLocked()
	s.gmailPending[pendingID] = gmailPendingEntry{
		label:        entry.label,
		email:        profile.EmailAddress,
		fromName:     entry.fromName,
		signature:    entry.signature,
		enc:          enc,
		folders:      len(folders),
		capabilities: capabilities,
		keyFP:        keyFingerprint(key),
		at:           time.Now(),
	}
	s.gmailPendingMu.Unlock()
	http.Redirect(w, r, "/?gmail_pending="+url.QueryEscape(pendingID)+"#/settings", http.StatusFound)
}
