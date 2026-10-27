package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const quotaUnitsPerUserMinute = 6000

func quotaCost(method, path string, query url.Values) int {
	switch {
	case path == "/profile" || path == "/labels":
		return 1
	case path == "/history":
		return 2
	case path == "/messages" && method == http.MethodGet:
		return 5
	case path == "/messages/send":
		return 100
	case path == "/drafts" && method == http.MethodPost:
		return 10
	case strings.HasSuffix(path, "/modify"):
		return 5
	case strings.HasPrefix(path, "/messages/"):
		if method == http.MethodDelete {
			return 10
		}
		switch query.Get("format") {
		case "raw", "full", "html":
			return 60
		}
		return 20
	}
	return 5
}

type quotaBudget struct {
	mu     sync.Mutex
	limit  float64
	window time.Duration
	tokens float64
	last   time.Time
}

func newQuotaBudget(limit int, window time.Duration) *quotaBudget {
	return &quotaBudget{limit: float64(limit), window: window, tokens: float64(limit), last: time.Now()}
}

func (b *quotaBudget) wait(ctx context.Context, cost int) error {
	for {
		b.mu.Lock()
		now := time.Now()
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.tokens += float64(elapsed) / float64(b.window) * b.limit
			if b.tokens > b.limit {
				b.tokens = b.limit
			}
			b.last = now
		}
		if b.tokens >= float64(cost) {
			b.tokens -= float64(cost)
			b.mu.Unlock()
			return nil
		}
		deficit := float64(cost) - b.tokens
		wait := time.Duration(deficit / b.limit * float64(b.window))
		b.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var userBudgets sync.Map

func budgetForUser(refreshToken string) *quotaBudget {
	sum := sha256.Sum256([]byte(refreshToken))
	key := hex.EncodeToString(sum[:])
	if v, ok := userBudgets.Load(key); ok {
		return v.(*quotaBudget)
	}
	b := newQuotaBudget(quotaUnitsPerUserMinute, time.Minute)
	actual, _ := userBudgets.LoadOrStore(key, b)
	return actual.(*quotaBudget)
}
