package proton

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/emersion/hydroxide/auth"
	"github.com/emersion/hydroxide/protonmail"

	"namakani/internal/mail"
	"namakani/internal/store"
)

type systemFolder struct {
	name    string
	special string
	label   string
}

var systemFolders = []systemFolder{
	{"INBOX", "inbox", protonmail.LabelInbox},
	{"All Mail", "all", protonmail.LabelAllMail},
	{"Archive", "archive", protonmail.LabelArchive},
	{"Drafts", "drafts", protonmail.LabelDraft},
	{"Starred", "flagged", protonmail.LabelStarred},
	{"Spam", "junk", protonmail.LabelSpam},
	{"Sent", "sent", protonmail.LabelSent},
	{"Trash", "trash", protonmail.LabelTrash},
}

var systemLabelByName = func() map[string]systemFolder {
	m := make(map[string]systemFolder, len(systemFolders))
	for _, f := range systemFolders {
		m[f.name] = f
	}
	return m
}()

var systemLabelIDs = func() map[string]bool {
	m := make(map[string]bool, len(systemFolders))
	for _, f := range systemFolders {
		m[f.label] = true
	}
	return m
}()

var locationLabels = map[string]bool{
	protonmail.LabelInbox:   true,
	protonmail.LabelTrash:   true,
	protonmail.LabelSpam:    true,
	protonmail.LabelArchive: true,
	protonmail.LabelSent:    true,
	protonmail.LabelDraft:   true,
}

type Session struct {
	st   *store.Store
	acct mail.Account

	mu    sync.Mutex
	c     *protonmail.Client
	keys  openpgp.EntityList
	addrs []*protonmail.Address

	cur     *folderCache
	folders map[string]*folderCache

	tmu     sync.Mutex
	tcond   *sync.Cond
	tholder int
	tusers  int
}

var (
	regMu     sync.Mutex
	regByAcct = make(map[int64]*Session)
)

func credFingerprint(acct mail.Account) string {
	sum := sha256.Sum256([]byte(acct.Username + "\x00" + acct.Password))
	return hex.EncodeToString(sum[:])
}

type AuthTerminalError struct {
	Reason string
	Err    error
}

func (e *AuthTerminalError) Error() string { return e.Err.Error() }
func (e *AuthTerminalError) Unwrap() error { return e.Err }

var apiCodePattern = regexp.MustCompile(`\[(\d+)\]`)

func humanVerificationReason(err error) string {
	return "Proton 要求完成人机验证。请点「重新连接」，在打开的窗口里输入密码并完成验证。"
}

func ClassifyError(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var term *AuthTerminalError
	if errors.As(err, &term) {
		return true, term.Reason
	}
	var apiErr *protonmail.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 9001:
			return true, humanVerificationReason(err)
		case 10013:
			return true, "保存的登录凭据已被拒绝（登录令牌已吊销），请点「重新连接」重新登录。"
		}
		return false, ""
	}
	if errors.Is(err, auth.ErrUnauthorized) {
		return true, "保存的登录凭据已被拒绝，请更新密码后点「重新连接」。"
	}
	msg := err.Error()
	if strings.Contains(msg, "two factor authentication enabled") {
		return true, "登录需要两步验证码。请点「重新连接」，在打开的窗口里输入验证码。"
	}
	if strings.Contains(msg, "cannot re-authenticate") {
		if strings.Contains(msg, "[9001]") {
			return true, humanVerificationReason(err)
		}
		if apiCodePattern.MatchString(msg) {
			return true, "保存的登录凭据已被拒绝，请更新密码后点「重新连接」。"
		}
	}
	return false, ""
}

func Connect(st *store.Store, acct mail.Account) (*Session, error) {
	regMu.Lock()
	defer regMu.Unlock()
	if s, ok := regByAcct[acct.ID]; ok && s.acct.Username == acct.Username &&
		credFingerprint(s.acct) == credFingerprint(acct) {
		return s, nil
	}
	mgr := auth.NewManager(newClient)
	c, keys, err := mgr.Auth(acct.Username, acct.Password)
	if err != nil {
		if _, reason := ClassifyError(err); reason != "" {
			return nil, &AuthTerminalError{Reason: reason, Err: err}
		}
		return nil, err
	}
	s := &Session{st: st, acct: acct, c: c, keys: keys}
	regByAcct[acct.ID] = s
	return s, nil
}

func (s *Session) addressesLocked() ([]*protonmail.Address, error) {
	if s.addrs != nil {
		return s.addrs, nil
	}
	addrs, err := s.c.ListAddresses()
	if err != nil {
		return nil, err
	}
	s.addrs = addrs
	return addrs, nil
}

func (s *Session) folderLabel(name string) (string, error) {
	if f, ok := systemLabelByName[name]; ok {
		return f.label, nil
	}
	labels, err := s.c.ListLabels()
	if err != nil {
		return "", err
	}
	for _, l := range labels {
		if l.Name == name {
			return l.ID, nil
		}
	}
	return "", errNoFolder(name)
}

type folderError string

func (e folderError) Error() string { return "proton: no such folder: " + string(e) }

func errNoFolder(name string) error { return folderError(name) }
