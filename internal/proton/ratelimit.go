package proton

import (
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

type rateLimitTransport struct {
	base http.RoundTripper
}

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	r := req
	for attempt := 0; ; attempt++ {
		resp, err := base.RoundTrip(r)
		if err != nil || attempt >= 3 ||
			(resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) {
			return resp, err
		}
		if req.Body != nil && req.GetBody == nil {
			return resp, nil
		}
		wait := retryAfterWait(resp.Header.Get("Retry-After"))
		wait += time.Duration(rand.Int63n(int64(9 * time.Second)))
		if wait > time.Minute {
			wait = time.Minute
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r2 := req.Clone(req.Context())
			r2.Body = body
			r = r2
		}
	}
}

func retryAfterWait(v string) time.Duration {
	if v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	return 10 * time.Second
}
