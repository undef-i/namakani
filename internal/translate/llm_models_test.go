package translate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type modelsCapture struct {
	method string
	path   string
	header http.Header
}

func modelsStubServer(t *testing.T, capReq *modelsCapture, status int, reply string) *httptest.Server {
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

func TestListModelsOpenAIShape(t *testing.T) {
	var capReq modelsCapture
	srv := modelsStubServer(t, &capReq, http.StatusOK, `{"data":[{"id":"gpt-x"},{"id":"gpt-y"}]}`)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "", "form-key")
	ids, err := eng.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(ids) != 2 || ids[0] != "gpt-x" || ids[1] != "gpt-y" {
		t.Fatalf("ids = %v, want [gpt-x gpt-y] in order", ids)
	}
	if capReq.method != http.MethodGet || capReq.path != "/v1/models" {
		t.Fatalf("request = %s %s, want GET /v1/models", capReq.method, capReq.path)
	}
}

func TestListModelsAnthropicShapeAndV1Base(t *testing.T) {
	var capReq modelsCapture
	srv := modelsStubServer(t, &capReq, http.StatusOK, `{"data":[{"id":"claude-x"}]}`)
	defer srv.Close()
	eng := NewLLMEngine("anthropic", srv.URL+"/v1", "", "ant-key")
	ids, err := eng.ListModels(context.Background())
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(ids) != 1 || ids[0] != "claude-x" {
		t.Fatalf("ids = %v", ids)
	}
	if capReq.path != "/v1/models" {
		t.Fatalf("path = %q, want /v1/models (no doubled /v1)", capReq.path)
	}
	if got := capReq.header.Get("X-Api-Key"); got != "ant-key" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := capReq.header.Get("Authorization"); got != "" {
		t.Fatalf("anthropic requests must not carry a bearer token, got %q", got)
	}
}

func TestListModelsUpstreamErrorPassesThrough(t *testing.T) {
	var capReq modelsCapture
	srv := modelsStubServer(t, &capReq, http.StatusUnauthorized, `{"error":"invalid api key"}`)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "", "bad-key")
	_, err := eng.ListModels(context.Background())
	if err == nil {
		t.Fatal("an upstream 401 must surface as an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error must carry the upstream status and body, got: %v", err)
	}
}
