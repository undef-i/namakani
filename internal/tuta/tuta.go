// Package tuta is a native Tuta Mail driver implementing the mail.Conn
// interface on top of Tuta's internal entity REST API and event WebSocket.
//
// The protocol implementation is a translation of the official Tuta client
// (tutao/tutanota, GPL-3.0) and its published compatibility test fixtures:
// password KDFs (bcrypt and Argon2id branches), the group key chain,
// per-instance session keys, the symmetric cipher formats (legacy CBC,
// CBC-then-HMAC and BLAKE3 AEAD) and the TutaCrypt hybrid encapsulation
// (X25519 + ML-KEM-1024) all follow the official sources named in
// THIRD-PARTY-NOTICES.TXT at the repository root.
package tuta

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"namakani/internal/mail"
	"namakani/internal/store"
)

const (
	apiBaseURL = "https://app.tuta.com"

	clientVersion = "362.261020.0"

	modelVersionSys      = "156"
	modelVersionTutanota = "115"
	modelVersionStorage  = "15"

	clientPlatformWeb = "7"
)

var ErrTOTPRequired = errors.New("tuta: totp required")

type RestError struct {
	StatusCode int
	Name       string
	Message    string
}

func (e *RestError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("tuta: %s (%d): %s", e.Name, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("tuta: http %d: %s", e.StatusCode, e.Message)
}

type Session struct {
	st   *store.Store
	acct mail.Account

	mu          sync.Mutex
	keysMu      sync.Mutex
	rest        *restClient
	accessToken string
	userID      string

	passphraseKey  []byte
	userGroupKey   []byte
	userGroupID    string
	groupKeys      map[string]map[int][]byte
	keyPairs       map[string]map[int]*groupKeyPair
	mailGroups     []string
	userKeyVersion int

	cur        *folderCache
	tokenCache *tokenCache
	folders    []tutaFolder
}

type groupKeyPair struct {
	x25519Pub  []byte
	x25519Priv []byte
	kyberPub   []byte
	kyberPriv  []byte
	rsaPub     []byte
}

var (
	regMu     sync.Mutex
	regByAcct = make(map[int64]*Session)
)

func credFingerprint(acct mail.Account) string {
	sum := sha256.Sum256([]byte(acct.Username + "\x00" + acct.Password))
	return hex.EncodeToString(sum[:])
}

func Connect(st *store.Store, acct mail.Account) (*Session, error) {
	regMu.Lock()
	defer regMu.Unlock()
	if s, ok := regByAcct[acct.ID]; ok && s.acct.Username == acct.Username &&
		credFingerprint(s.acct) == credFingerprint(acct) {
		return s, nil
	}
	s := newSession()
	s.st = st
	s.acct = acct
	if err := s.resume(); err != nil {
		if err := s.login(acct.Password, ""); err != nil {
			return nil, err
		}
	}
	regByAcct[acct.ID] = s
	return s, nil
}

func newSession() *Session {
	return &Session{
		rest:      newRestClient(),
		groupKeys: make(map[string]map[int][]byte),
		keyPairs:  make(map[string]map[int]*groupKeyPair),
		tokenCache: &tokenCache{
			archives: make(map[string]*blobAccessInfo),
			writes:   make(map[string]*blobAccessInfo),
		},
	}
}

func Authorize(email, password, totp string) (userID, accessToken string, err error) {
	s := newSession()
	s.acct.Email = email
	s.acct.Username = email
	if err := s.login(password, totp); err != nil {
		return "", "", err
	}
	return s.userID, s.accessToken, nil
}
