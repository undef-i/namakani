package avatar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Resolver struct {
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	Fetch     func(ctx context.Context, url string) (int, string, []byte, error)
}

func DefaultResolver() *Resolver {
	client := &http.Client{Timeout: 6 * time.Second}
	return &Resolver{
		LookupTXT: func(ctx context.Context, name string) ([]string, error) {
			return net.DefaultResolver.LookupTXT(ctx, name)
		},
		Fetch: func(ctx context.Context, url string) (int, string, []byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return 0, "", nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, "", nil, err
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return 0, "", nil, err
			}
			return resp.StatusCode, resp.Header.Get("Content-Type"), body, nil
		},
	}
}

func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func GravatarHash(email string) string {
	sum := sha256.Sum256([]byte(NormalizeEmail(email)))
	return hex.EncodeToString(sum[:])
}

func GravatarURL(email string) string {
	return "https://www.gravatar.com/avatar/" + GravatarHash(email) + "?d=404&s=128"
}

func ParseBIMILogoURL(txts []string) (string, bool) {
	for _, rec := range txts {
		rec = strings.TrimSpace(rec)
		if !strings.HasPrefix(rec, "v=BIMI1") {
			continue
		}
		for _, part := range strings.Split(rec, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "l=") {
				u := strings.TrimSpace(strings.TrimPrefix(part, "l="))
				if strings.HasPrefix(u, "https://") {
					return u, true
				}
				return "", false
			}
		}
	}
	return "", false
}

func (r *Resolver) Resolve(ctx context.Context, email string) (string, string, []byte) {
	norm := NormalizeEmail(email)
	at := strings.LastIndex(norm, "@")
	if at < 0 || at == len(norm)-1 {
		return "", "", nil
	}
	domain := norm[at+1:]
	if txts, err := r.LookupTXT(ctx, "default._bimi."+domain); err == nil {
		if logoURL, ok := ParseBIMILogoURL(txts); ok {
			if status, _, body, err := r.Fetch(ctx, logoURL); err == nil && status == http.StatusOK && len(body) > 0 {
				if clean, ok := SanitizeSVG(body); ok {
					return "bimi", "image/svg+xml", clean
				}
			}
		}
	}
	if status, mime, body, err := r.Fetch(ctx, GravatarURL(norm)); err == nil && status == http.StatusOK && len(body) > 0 {
		if !strings.HasPrefix(mime, "image/") {
			mime = http.DetectContentType(body)
		}
		if strings.HasPrefix(mime, "image/") {
			return "gravatar", mime, body
		}
	}
	return "", "", nil
}
