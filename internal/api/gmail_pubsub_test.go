package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func pubSubEnvelope(t *testing.T, email, historyID string) string {
	t.Helper()
	note, err := json.Marshal(map[string]any{"emailAddress": email, "historyId": json.Number(historyID)})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{
		"message": map[string]any{
			"data":        base64.StdEncoding.EncodeToString(note),
			"messageId":   "9876543210123456",
			"publishTime": "2026-11-03T15:21:00.123Z",
			"attributes":  map[string]string{"origin": "gmail.googleapis.com"},
		},
		"subscription": "projects/test-project/subscriptions/gmail-push-sub",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func postPubSub(t *testing.T, base, token, body string) int {
	t.Helper()
	resp, err := http.Post(base+"/api/gmail/pubsub?token="+url.QueryEscape(token), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post pubsub: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestGmailPubSubRealEnvelopeWakesParkedSession(t *testing.T) {
	fake := newAPIFakeGmail(t)
	f := newGmailFixture(t)
	cfg := f.st.AppSettings()
	cfg.BaseURL = "https://mail.example.com"
	if err := f.st.SaveAppSettings(cfg); err != nil {
		t.Fatalf("save base url: %v", err)
	}
	f.insertGmailAccount(t)

	acct, err := f.srv.loadMailAccount(1, f.key)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	f.srv.mgr.Start(acct)
	waitForCond(t, "the session to park on the missing topic", func() bool {
		st := f.srv.mgr.Status(1)
		return st.State == "error" && strings.Contains(st.LastError, "topic not configured")
	})
	if n := fake.watchCount(); n != 0 {
		t.Fatalf("watch calls while parked = %d; want 0", n)
	}

	const topic = "projects/test-project/topics/gmail-push"
	resp := f.doReq(t, http.MethodPut, "/api/settings/gmail-oauth",
		`{"client_id":"cid-1","client_secret":"","pubsub_topic":"`+topic+`"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put gmail oauth status = %d; want 200", resp.StatusCode)
	}
	waitForCond(t, "the parked session to wake and register watch", func() bool {
		return fake.watchCount() >= 1 && f.srv.mgr.Status(1).State == "idle"
	})
	if got := fake.watchedTopic(); got != topic {
		t.Fatalf("watch topic = %q; want %q", got, topic)
	}

	endpoint := f.srv.gmailPubSubEndpoint()
	ep, err := url.Parse(endpoint)
	if err != nil || ep.Query().Get("token") == "" {
		t.Fatalf("pubsub endpoint = %q; want a token query", endpoint)
	}
	token := ep.Query().Get("token")

	base := fake.historyCount()
	if code := postPubSub(t, f.http.URL, token, pubSubEnvelope(t, "user@gmail.com", "1001")); code != http.StatusOK {
		t.Fatalf("real-shaped envelope status = %d; want 200", code)
	}
	waitForCond(t, "the push to trigger a history sync", func() bool {
		return fake.historyCount() > base
	})

	base = fake.historyCount()
	if code := postPubSub(t, f.http.URL, token, pubSubEnvelope(t, "nobody@example.com", "1002")); code != http.StatusOK {
		t.Fatalf("unknown-mailbox envelope status = %d; want 200", code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := fake.historyCount(); n != base {
		t.Fatalf("history calls after unknown-mailbox push = %d; want %d (no wake)", n, base)
	}

	if code := postPubSub(t, f.http.URL, token, `{"message":`); code != http.StatusBadRequest {
		t.Fatalf("truncated envelope status = %d; want 400 so pub/sub redelivers", code)
	}
	badData := `{"message":{"data":"!!!not-base64!!!","messageId":"1","publishTime":"2026-11-03T15:21:00.123Z"},"subscription":"projects/test-project/subscriptions/gmail-push-sub"}`
	if code := postPubSub(t, f.http.URL, token, badData); code != http.StatusBadRequest {
		t.Fatalf("undecodable data status = %d; want 400 so pub/sub redelivers", code)
	}
	noEmail := `{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(`{"historyId":"7"}`)) + `","messageId":"1"},"subscription":"projects/test-project/subscriptions/gmail-push-sub"}`
	if code := postPubSub(t, f.http.URL, token, noEmail); code != http.StatusBadRequest {
		t.Fatalf("notification without email status = %d; want 400 so pub/sub redelivers", code)
	}
	if code := postPubSub(t, f.http.URL, "wrong-token", pubSubEnvelope(t, "user@gmail.com", "1003")); code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d; want 403", code)
	}
}
