package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type llmModelsCapture struct {
	method string
	path   string
	header http.Header
}

func llmModelsUpstream(t *testing.T, capReq *llmModelsCapture, status int, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capReq.method = r.Method
		capReq.path = r.URL.Path
		capReq.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
}

func postLLMModels(t *testing.T, f *drainFixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := f.authedReq(http.MethodPost, "/api/translate/llm/models", body)
	rec := httptest.NewRecorder()
	f.srv.handleLLMModels(rec, req)
	return rec
}

func decodeModelIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Models
}

func TestLLMModelsEndpointOpenAIShape(t *testing.T) {
	f := newDrainFixture(t)
	var capReq llmModelsCapture
	up := llmModelsUpstream(t, &capReq, http.StatusOK, `{"data":[{"id":"m-one"},{"id":"m-two"}]}`)
	defer up.Close()
	rec := postLLMModels(t, f, `{"llm_format":"openai","llm_endpoint":"`+up.URL+`","llm_api_key":"form-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	ids := decodeModelIDs(t, rec)
	if len(ids) != 2 || ids[0] != "m-one" || ids[1] != "m-two" {
		t.Fatalf("models = %v", ids)
	}
	if got := capReq.header.Get("Authorization"); got != "Bearer form-key" {
		t.Fatalf("authorization = %q, want the key from the form", got)
	}
}

func TestLLMModelsEndpointAnthropicShape(t *testing.T) {
	f := newDrainFixture(t)
	var capReq llmModelsCapture
	up := llmModelsUpstream(t, &capReq, http.StatusOK, `{"data":[{"id":"claude-x"}]}`)
	defer up.Close()
	rec := postLLMModels(t, f, `{"llm_format":"anthropic","llm_endpoint":"`+up.URL+`","llm_api_key":"ant-key"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	ids := decodeModelIDs(t, rec)
	if len(ids) != 1 || ids[0] != "claude-x" {
		t.Fatalf("models = %v", ids)
	}
	if got := capReq.header.Get("X-Api-Key"); got != "ant-key" {
		t.Fatalf("x-api-key = %q", got)
	}
}

func TestLLMModelsEndpointUsesSavedKeyWhenFormKeyEmpty(t *testing.T) {
	f := newDrainFixture(t)
	f.srv.serverKey = []byte("0123456789abcdef0123456789abcdef")
	put := f.authedReq(http.MethodPut, "/api/settings/translate",
		`{"engine":"llm","target":"auto","llm_format":"openai","llm_endpoint":"https://llm.example","llm_model":"m","llm_api_key":"saved-key"}`)
	rec := httptest.NewRecorder()
	f.srv.handlePutTranslateSettings(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings status = %d", rec.Code)
	}
	var capReq llmModelsCapture
	up := llmModelsUpstream(t, &capReq, http.StatusOK, `{"data":[{"id":"m"}]}`)
	defer up.Close()
	rec2 := postLLMModels(t, f, `{"llm_format":"openai","llm_endpoint":"`+up.URL+`","llm_api_key":""}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec2.Code, rec2.Body.String())
	}
	if got := capReq.header.Get("Authorization"); got != "Bearer saved-key" {
		t.Fatalf("authorization = %q, want the saved key when the form key is empty", got)
	}
}

func TestLLMModelsEndpointUpstreamErrorIsMapped(t *testing.T) {
	f := newDrainFixture(t)
	var capReq llmModelsCapture
	up := llmModelsUpstream(t, &capReq, http.StatusUnauthorized, `{"error":"invalid api key"}`)
	defer up.Close()
	rec := postLLMModels(t, f, `{"llm_format":"openai","llm_endpoint":"`+up.URL+`","llm_api_key":"bad"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("an upstream 401 must not be reported as success: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "invalid api key") {
		t.Fatalf("upstream error text must not reach the client, got: %s", rec.Body.String())
	}
}

