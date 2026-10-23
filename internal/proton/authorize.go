package proton

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersion/hydroxide/auth"
	"github.com/emersion/hydroxide/protonmail"
)

var ErrTOTPRequired = errors.New("totp required")

type CaptchaError struct {
	Token string
	URL   string
}

func (e *CaptchaError) Error() string { return "captcha verification required" }

type captchaChallenge struct {
	token  string
	webURL string
}

type hvState struct {
	armedToken string
	challenge  *captchaChallenge
}

type hvTransport struct {
	base  http.RoundTripper
	state *hvState
}

func (t *hvTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.state.armedToken != "" {
		r := req.Clone(req.Context())
		r.Header.Set("x-pm-human-verification-token", t.state.armedToken)
		r.Header.Set("x-pm-human-verification-token-type", "captcha")
		req = r
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		return resp, nil
	}
	var parsed struct {
		Code    int `json:"Code"`
		Details *struct {
			Methods []string `json:"HumanVerificationMethods"`
			Token   string   `json:"HumanVerificationToken"`
			WebURL  string   `json:"WebUrl"`
		} `json:"Details"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Code != 9001 || parsed.Details == nil || parsed.Details.Token == "" {
		return resp, nil
	}
	for _, m := range parsed.Details.Methods {
		if m == "captcha" {
			t.state.challenge = &captchaChallenge{token: parsed.Details.Token, webURL: parsed.Details.WebURL}
			break
		}
	}
	return resp, nil
}

func verifyURL(rootURL string, ch *captchaChallenge) (string, bool) {
	var origin *url.URL
	if ch.webURL != "" {
		if u, err := url.Parse(ch.webURL); err == nil && u.Scheme != "" && u.Host != "" {
			origin = &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
		}
	}
	if origin == nil {
		u, err := url.Parse(rootURL)
		if err != nil {
			return "", false
		}
		host := u.Hostname()
		dot := strings.Index(host, ".")
		if dot < 0 {
			return "", false
		}
		origin = &url.URL{Scheme: u.Scheme, Host: "verify." + host[dot+1:], Path: "/"}
	}
	q := url.Values{}
	q.Set("methods", "captcha")
	q.Set("token", ch.token)
	origin.RawQuery = q.Encode()
	return origin.String(), true
}

func Init(dataDir string) {
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(dataDir, "hydroxide"))
}

func newClient() *protonmail.Client {
	return &protonmail.Client{
		RootURL:    "https://mail.proton.me/api",
		AppVersion: "Other",
		HTTPClient: &http.Client{Transport: rateLimitTransport{base: http.DefaultTransport}},
	}
}

func Authorize(username, loginPassword, mailboxPassword, totp, hvToken string) (string, error) {
	state := &hvState{armedToken: hvToken}
	c := newClient()
	c.HTTPClient = &http.Client{Transport: &hvTransport{base: rateLimitTransport{base: http.DefaultTransport}, state: state}}
	bridgePassword, err := authorize(c, username, loginPassword, mailboxPassword, totp)
	if err != nil && state.challenge != nil {
		if u, ok := verifyURL(c.RootURL, state.challenge); ok {
			return "", &CaptchaError{Token: state.challenge.token, URL: u}
		}
	}
	return bridgePassword, err
}

func authorize(c *protonmail.Client, username, loginPassword, mailboxPassword, totp string) (string, error) {
	info, err := c.AuthInfo(username)
	if err != nil {
		return "", err
	}
	a, err := c.Auth(username, loginPassword, info)
	if err != nil {
		return "", err
	}
	if a.TwoFactor.Enabled != 0 {
		if a.TwoFactor.TOTP != 1 {
			return "", errors.New("only TOTP is supported as a 2FA method")
		}
		if totp == "" {
			return "", ErrTOTPRequired
		}
		scope, err := c.AuthTOTP(totp)
		if err != nil {
			return "", err
		}
		a.Scope = scope
	}
	if mailboxPassword == "" || a.PasswordMode == protonmail.PasswordSingle {
		mailboxPassword = loginPassword
	}
	keySalts, err := c.ListKeySalts()
	if err != nil {
		return "", err
	}
	if _, err := c.Unlock(a, keySalts, mailboxPassword); err != nil {
		return "", err
	}
	secretKey, bridgePassword, err := auth.GeneratePassword()
	if err != nil {
		return "", err
	}
	if err := auth.EncryptAndSave(&auth.CachedAuth{
		Auth:            *a,
		LoginPassword:   loginPassword,
		MailboxPassword: mailboxPassword,
		KeySalts:        keySalts,
	}, username, secretKey); err != nil {
		return "", err
	}
	return bridgePassword, nil
}
