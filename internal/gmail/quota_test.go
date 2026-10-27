package gmail

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestClassifyErrorQuotaIsTemporary(t *testing.T) {
	for _, reason := range []string{"rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded"} {
		err := &APIError{Status: 403, Reason: reason, Message: "quota"}
		if permanent, gone := ClassifyError(err); permanent || gone {
			t.Fatalf("reason %s: got (permanent=%v, gone=%v), want a temporary error", reason, permanent, gone)
		}
		if !IsQuotaError(err) {
			t.Fatalf("reason %s: IsQuotaError = false, want true", reason)
		}
	}
	err := &APIError{Status: 403, Reason: "forbidden", Message: "nope"}
	if permanent, _ := ClassifyError(err); !permanent {
		t.Fatal("plain 403 must stay permanent")
	}
	if IsAuthError(&APIError{Status: 403, Reason: "rateLimitExceeded"}) {
		t.Fatal("quota 403 must not read as an auth error")
	}
}

func TestQuotaBudgetPacesSpend(t *testing.T) {
	b := newQuotaBudget(40, 300*time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := b.wait(ctx, 20); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("spend within budget blocked for %v", elapsed)
	}
	if err := b.wait(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("third spend of 20 against a 40 per 300ms budget returned after %v, want a pacing wait", elapsed)
	}
}

func TestQuotaBudgetShapesClientCalls(t *testing.T) {
	f := newFakeGmail(t)
	f.use(t)
	f.addMessage(stdMsg("m1", "INBOX"))
	cl := NewClient(testCreds())
	cl.budget = newQuotaBudget(45, 400*time.Millisecond)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := cl.GetMeta(context.Background(), "m1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.countCalls("GET /gmail/v1/users/me/messages/m1"); got != 3 {
		t.Fatalf("server saw %d gets, want 3", got)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("3 metadata gets cost 60 against a 45 per 400ms budget but finished in %v", elapsed)
	}
}

func TestQuotaCostTable(t *testing.T) {
	cases := []struct {
		method, path string
		format       string
		want         int
	}{
		{"GET", "/profile", "", 1},
		{"GET", "/labels", "", 1},
		{"GET", "/messages", "", 5},
		{"GET", "/messages/m1", "metadata", 20},
		{"GET", "/messages/m1", "raw", 60},
		{"GET", "/history", "", 2},
		{"POST", "/messages/m1/modify", "", 5},
		{"DELETE", "/messages/m1", "", 10},
		{"POST", "/messages/send", "", 100},
		{"POST", "/drafts", "", 10},
	}
	for _, c := range cases {
		q := url.Values{}
		if c.format != "" {
			q.Set("format", c.format)
		}
		if got := quotaCost(c.method, c.path, q); got != c.want {
			t.Errorf("%s %s format=%s: cost %d, want %d", c.method, c.path, c.format, got, c.want)
		}
	}
}
