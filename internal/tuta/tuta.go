// Package tuta is a native Tuta Mail driver implementing the mail.Conn
// interface on top of Tuta's internal entity REST API and event WebSocket.
// The protocol implementation follows the official Tuta client; see
// THIRD-PARTY-NOTICES.TXT at the repository root.
package tuta

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"namakani/internal/mail"
	"namakani/internal/store"
)

const loginPhaseTimeout = 45 * time.Second

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
	evState        *eventState
	groupKeys      map[string]map[int][]byte
	keyPairs       map[string]map[int]*groupKeyPair
	mailGroups     []string
	userKeyVersion int

	cur        *folderCache
	tokenCache *tokenCache

	tmu     sync.Mutex
	tcond   *sync.Cond
	tholder int
	tusers  int
	folders    []tutaFolder
	instKeys   map[string][]byte
	details    map[string]instance
}

type groupKeyPair struct {
	x25519Pub  []byte
	x25519Priv []byte
	kyberPub   []byte
	kyberPriv  []byte
	rsaPub     []byte
	rsaPriv    *rsa.PrivateKey
}

var (
	regMu     sync.Mutex
	regByAcct = make(map[int64]*Session)
)

func credFingerprint(acct mail.Account) string {
	sum := sha256.Sum256([]byte(acct.Username + "\x00" + acct.Password))
	return hex.EncodeToString(sum[:])
}

func (s *Session) beginPhase(d time.Duration) func() {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	s.rest.ctx = ctx
	return func() {
		cancel()
		s.rest.ctx = nil
	}
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
	start := time.Now()
	endPhase := s.beginPhase(loginPhaseTimeout)
	via := "resume"
	err := s.resume()
	if err != nil {
		via = "login"
		err = s.login(acct.Password, "")
	}
	endPhase()
	if err != nil {
		log.Printf("tuta: connect account %d via %s failed after %s: %v",
			acct.ID, via, time.Since(start).Round(time.Millisecond), err)
		return nil, err
	}
	log.Printf("tuta: connect account %d via %s ok in %s",
		acct.ID, via, time.Since(start).Round(time.Millisecond))
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
		details: make(map[string]instance),
	}
}

func LoginSession(email, password, totp string) (*Session, error) {
	s := newSession()
	s.acct.Email = email
	s.acct.Username = email
	start := time.Now()
	endPhase := s.beginPhase(loginPhaseTimeout)
	err := s.login(password, totp)
	endPhase()
	if err != nil {
		log.Printf("tuta: authorize failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
		return nil, err
	}
	log.Printf("tuta: authorize ok in %s", time.Since(start).Round(time.Millisecond))
	return s, nil
}

func Authorize(email, password, totp string) (userID, accessToken string, err error) {
	s, err := LoginSession(email, password, totp)
	if err != nil {
		return "", "", err
	}
	return s.userID, s.accessToken, nil
}
