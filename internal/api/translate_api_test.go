package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"namakani/internal/translate"
)

type stubTranslateEngine struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (e *stubTranslateEngine) Key() string { return "stub-engine" }

func (e *stubTranslateEngine) Translate(ctx context.Context, segs []translate.Segment, src, tgt string) ([]translate.Segment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	if e.fail {
		return nil, errors.New("stub engine down")
	}
	out := make([]translate.Segment, len(segs))
	for i, s := range segs {
		out[i] = translate.Segment{ID: s.ID, Text: "T[" + s.Text + "]"}
	}
	return out, nil
}

func (e *stubTranslateEngine) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func seedBodyMessage(t *testing.T, f *drainFixture, id int64, subject, text, html string) {
	t.Helper()
	f.addMessage(t, id, 1, "INBOX", 100+uint32(id), 0)
	if _, err := f.st.DB.Exec("UPDATE messages SET subject = ? WHERE id = ?", subject, id); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveBody(id, text, html, "snip", false); err != nil {
		t.Fatal(err)
	}
}

func postTranslate(t *testing.T, f *drainFixture, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := f.authedReq(http.MethodPost, "/api/messages/10/translate", body)
	req.SetPathValue("id", "10")
	rec := httptest.NewRecorder()
	f.srv.handleTranslateMessage(rec, req)
	return rec
}

func TestTranslateEndpointTranslatesAndCaches(t *testing.T) {
	f := newDrainFixture(t)
	seedBodyMessage(t, f, 10, "October invoice", "Your invoice is ready.", "<p>Your invoice is ready.</p>")
	stub := &stubTranslateEngine{}
	f.srv.translateBuild = func(cfg translateSettings) (translate.Engine, error) { return stub, nil }
	rec := postTranslate(t, f, 10, `{"target":"zh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("translate status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Subject string `json:"subject"`
		HTML    string `json:"html"`
		SrcLang string `json:"src_lang"`
		Cached  bool   `json:"cached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Subject, "T[October invoice]") {
		t.Fatalf("subject not translated: %q", resp.Subject)
	}
	if !strings.Contains(resp.HTML, "T[") || resp.SrcLang != "en" {
		t.Fatalf("body not translated or src wrong: %+v", resp)
	}
	if resp.Cached {
		t.Fatal("first translation must not claim to be cached")
	}
	calls := stub.callCount()
	rec2 := postTranslate(t, f, 10, `{"target":"zh"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second translate status = %d", rec2.Code)
	}
	var resp2 struct {
		Cached bool `json:"cached"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if !resp2.Cached {
		t.Fatal("second translation of the same mail must come from the cache")
	}
	if stub.callCount() != calls {
		t.Fatalf("engine was called again for a cached mail: %d -> %d", calls, stub.callCount())
	}
}

func TestTranslateEndpointFailureWritesNoCache(t *testing.T) {
	f := newDrainFixture(t)
	seedBodyMessage(t, f, 10, "October invoice", "Your invoice is ready.", "<p>Your invoice is ready.</p>")
	stub := &stubTranslateEngine{fail: true}
	f.srv.translateBuild = func(cfg translateSettings) (translate.Engine, error) { return stub, nil }
	rec := postTranslate(t, f, 10, `{"target":"zh"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failing engine must give 502, got %d body %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := f.st.DB.QueryRow("SELECT COUNT(*) FROM translations WHERE message_id = 10").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a failed translation left %d cache rows", n)
	}
}

func TestTranslateEndpointSameLanguageSkipsEngine(t *testing.T) {
	f := newDrainFixture(t)
	seedBodyMessage(t, f, 10, "十月份发票", "您的发票已经准备好了。", "<p>您的发票已经准备好了。</p>")
	stub := &stubTranslateEngine{}
	f.srv.translateBuild = func(cfg translateSettings) (translate.Engine, error) { return stub, nil }
	rec := postTranslate(t, f, 10, `{"target":"zh"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SameLanguage bool   `json:"same_language"`
		Subject      string `json:"subject"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.SameLanguage {
		t.Fatalf("chinese mail to chinese target must be same_language: %s", rec.Body.String())
	}
	if resp.Subject != "十月份发票" {
		t.Fatalf("subject changed: %q", resp.Subject)
	}
	if stub.callCount() != 0 {
		t.Fatal("engine must not run when the mail is already in the target language")
	}
}

func TestTranslateSettingsRoundTripAndKeySealed(t *testing.T) {
	f := newDrainFixture(t)
	f.srv.serverKey = []byte("0123456789abcdef0123456789abcdef")
	put := f.authedReq(http.MethodPut, "/api/settings/translate",
		`{"engine":"llm","target":"zh","llm_format":"anthropic","llm_endpoint":"https://llm.example","llm_model":"model-x","llm_api_key":"my-secret-key"}`)
	rec := httptest.NewRecorder()
	f.srv.handlePutTranslateSettings(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings status = %d body %s", rec.Code, rec.Body.String())
	}
	get := f.authedReq(http.MethodGet, "/api/settings/translate", "")
	rec2 := httptest.NewRecorder()
	f.srv.handleGetTranslateSettings(rec2, get)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", rec2.Code)
	}
	body := rec2.Body.String()
	var resp struct {
		Settings translateSettings `json:"settings"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	cfg := resp.Settings
	if !cfg.HasKey {
		t.Fatal("has_key must be true after saving a key")
	}
	if strings.Contains(body, "my-secret-key") {
		t.Fatal("the api key leaked into the settings response")
	}
	raw, ok, _ := f.st.GetSetting(settingTrLLMKeyEnc)
	if !ok || raw == "" {
		t.Fatal("encrypted key was not stored")
	}
	if strings.Contains(raw, "my-secret-key") {
		t.Fatal("the api key is stored in plaintext")
	}
	_, plain := f.srv.loadTranslateSettings()
	if plain != "my-secret-key" {
		t.Fatalf("decrypted key = %q", plain)
	}
	put2 := f.authedReq(http.MethodPut, "/api/settings/translate",
		`{"engine":"google","target":"auto","llm_format":"openai","llm_endpoint":"","llm_model":"","llm_api_key":""}`)
	rec3 := httptest.NewRecorder()
	f.srv.handlePutTranslateSettings(rec3, put2)
	if rec3.Code != http.StatusOK {
		t.Fatalf("second put status = %d", rec3.Code)
	}
	_, plain2 := f.srv.loadTranslateSettings()
	if plain2 != "my-secret-key" {
		t.Fatal("saving settings with an empty key field must keep the stored key")
	}
}

func TestAvatarEndpointsCacheAndServe(t *testing.T) {
	f := newDrainFixture(t)
	calls := 0
	f.srv.avatarResolve = func(ctx context.Context, email string) (string, string, []byte) {
		calls++
		if email == "known@x.test" {
			return "gravatar", "image/png", []byte("PNGBYTES")
		}
		return "", "", nil
	}
	post := func(emails string) *httptest.ResponseRecorder {
		req := f.authedReq(http.MethodPost, "/api/avatars", `{"emails":[`+emails+"]}")
		rec := httptest.NewRecorder()
		f.srv.handleAvatars(rec, req)
		return rec
	}
	rec := post(`"known@x.test","ghost@x.test"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("avatars status = %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Avatars map[string]string `json:"avatars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Avatars["known@x.test"] != "gravatar" || resp.Avatars["ghost@x.test"] != "" {
		t.Fatalf("avatar results wrong: %+v", resp.Avatars)
	}
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2", calls)
	}
	rec = post(`"known@x.test","ghost@x.test"`)
	if calls != 2 {
		t.Fatalf("cached avatars (hit and miss) must not re-resolve, calls = %d", calls)
	}
	imgReq := f.authedReq(http.MethodGet, "/api/avatar/img?e=known@x.test", "")
	imgRec := httptest.NewRecorder()
	f.srv.handleAvatarImage(imgRec, imgReq)
	if imgRec.Code != http.StatusOK || imgRec.Body.String() != "PNGBYTES" {
		t.Fatalf("img = %d %q", imgRec.Code, imgRec.Body.String())
	}
	if ct := imgRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("img content type = %q", ct)
	}
	imgReq2 := f.authedReq(http.MethodGet, "/api/avatar/img?e=ghost@x.test", "")
	imgRec2 := httptest.NewRecorder()
	f.srv.handleAvatarImage(imgRec2, imgReq2)
	if imgRec2.Code != http.StatusNotFound {
		t.Fatalf("sender without an image must 404, got %d", imgRec2.Code)
	}
}

func TestTranslateSettingsLocalNormalizedToGoogle(t *testing.T) {
	f := newDrainFixture(t)
	if err := f.srv.st.SetSetting(settingTrEngine, "local"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.srv.loadTranslateSettings()
	if cfg.Engine != "google" {
		t.Fatalf("stored local must load as google, got %q", cfg.Engine)
	}
	if v, ok, _ := f.srv.st.GetSetting(settingTrEngine); !ok || v != "google" {
		t.Fatalf("stored engine after load = %q (ok=%v), want google persisted", v, ok)
	}
}
