package gmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	APIBase  = "https://gmail.googleapis.com/gmail/v1/users/me"
	TokenURL = "https://oauth2.googleapis.com/token"
	AuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
)

const Scope = "https://mail.google.com/"

type Creds struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

func (c Creds) JSON() string {
	b, _ := json.Marshal(c)
	return string(b)
}

func TokenCredsJSON(refreshToken string) string {
	b, _ := json.Marshal(struct {
		RefreshToken string `json:"refresh_token"`
	}{RefreshToken: refreshToken})
	return string(b)
}

func ParseCreds(password string) (Creds, error) {
	var c Creds
	if err := json.Unmarshal([]byte(password), &c); err != nil {
		return Creds{}, fmt.Errorf("gmail: stored credentials are not readable: %w", err)
	}
	if c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "" {
		return Creds{}, errors.New("gmail: stored credentials are incomplete")
	}
	return c, nil
}

type APIError struct {
	Status  int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gmail: api error %d %s: %s", e.Status, e.Reason, e.Message)
}

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 404
}

func IsAuthError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == 401 || (ae.Status == 400 && ae.Reason == "invalid_grant")
}

func IsQuotaError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 {
		return false
	}
	switch ae.Reason {
	case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded":
		return true
	}
	return false
}

func ClassifyError(err error) (permanent, gone bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 404:
			return false, true
		case IsQuotaError(err):
			return false, false
		case ae.Status == 401 || ae.Status == 403:
			return true, false
		case ae.Status == 429 || ae.Status >= 500:
			return false, false
		default:
			return true, false
		}
	}
	return false, false
}

type Client struct {
	mu        sync.Mutex
	creds     Creds
	access    string
	accessExp time.Time
	http      *http.Client
	now       func() time.Time
	budget    *quotaBudget
}

func NewClient(creds Creds) *Client {
	return &Client{creds: creds, http: &http.Client{}, now: time.Now, budget: budgetForUser(creds.RefreshToken)}
}

func (c *Client) Creds() Creds {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creds
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func postToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, body)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("gmail: token response is not readable: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("gmail: token response carries no access token")
	}
	return &tr, nil
}

func parseAPIError(status int, body []byte) error {
	ae := &APIError{Status: status}
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		ae.Message = env.Error.Message
		ae.Reason = env.Error.Status
		if len(env.Error.Errors) > 0 && env.Error.Errors[0].Reason != "" {
			ae.Reason = env.Error.Errors[0].Reason
		}
		return ae
	}
	var oauthEnv struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &oauthEnv); err == nil && oauthEnv.Error != "" {
		ae.Reason = oauthEnv.Error
		ae.Message = oauthEnv.Description
		return ae
	}
	ae.Message = strings.TrimSpace(string(body))
	return ae
}

func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.access != "" && c.now().Before(c.accessExp) {
		return c.access, nil
	}
	form := url.Values{
		"client_id":     {c.creds.ClientID},
		"client_secret": {c.creds.ClientSecret},
		"refresh_token": {c.creds.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	tr, err := postToken(ctx, form)
	if err != nil {
		return "", err
	}
	c.access = tr.AccessToken
	c.accessExp = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.RefreshToken != "" {
		c.creds.RefreshToken = tr.RefreshToken
	}
	return c.access, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, int, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = b
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.budget.wait(ctx, quotaCost(method, path, query)); err != nil {
			return nil, 0, err
		}
		token, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return nil, 0, err
		}
		u := APIBase + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		var rdr io.Reader
		if payload != nil {
			rdr = strings.NewReader(string(payload))
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, 0, err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, resp.StatusCode, parseAPIError(resp.StatusCode, data)
		}
		return data, resp.StatusCode, nil
	}
	return nil, 0, errors.New("gmail: request failed")
}

func PKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func RandomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func AuthCodeURL(clientID, redirectURI, state, challenge, loginHint string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {Scope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return AuthURL + "?" + q.Encode()
}

func ExchangeCode(ctx context.Context, clientID, clientSecret, code, verifier, redirectURI string) (Creds, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	tr, err := postToken(ctx, form)
	if err != nil {
		return Creds{}, err
	}
	if tr.RefreshToken == "" {
		return Creds{}, errors.New("gmail: Google did not return a refresh token")
	}
	return Creds{ClientID: clientID, ClientSecret: clientSecret, RefreshToken: tr.RefreshToken}, nil
}
