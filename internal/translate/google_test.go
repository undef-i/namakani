package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGoogleEngineAlignsResultsAndSendsShape(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode([]string{"译文甲", "译文乙"})
	}))
	defer srv.Close()
	eng := NewGoogleEngine()
	eng.BaseURL = srv.URL
	out, err := eng.Translate(context.Background(), []Segment{
		{ID: 7, Text: "First one."},
		{ID: 9, Text: "Second one."},
	}, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 || out[0].ID != 7 || out[0].Text != "译文甲" || out[1].ID != 9 || out[1].Text != "译文乙" {
		t.Fatalf("misaligned results: %+v", out)
	}
	if gotQuery.Get("client") != "dict-chrome-ex" {
		t.Fatalf("client param = %q", gotQuery.Get("client"))
	}
	if gotQuery.Get("sl") != "en" || gotQuery.Get("tl") != "zh-CN" {
		t.Fatalf("language params sl=%q tl=%q", gotQuery.Get("sl"), gotQuery.Get("tl"))
	}
	qs := gotQuery["q"]
	if len(qs) != 2 || qs[0] != "First one." || qs[1] != "Second one." {
		t.Fatalf("q params = %q", qs)
	}
}

func TestGoogleEngineCountMismatchIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]string{"只有一条"})
	}))
	defer srv.Close()
	eng := NewGoogleEngine()
	eng.BaseURL = srv.URL
	_, err := eng.Translate(context.Background(), []Segment{
		{ID: 1, Text: "One."},
		{ID: 2, Text: "Two."},
	}, "en", "zh")
	if err == nil {
		t.Fatal("a short result set must be an error, not a silent misalignment")
	}
	if !strings.Contains(err.Error(), "count") {
		t.Fatalf("error should describe the count mismatch: %v", err)
	}
}

func TestGoogleEngineHTTPFailureIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	eng := NewGoogleEngine()
	eng.BaseURL = srv.URL
	_, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if err == nil {
		t.Fatal("HTTP 429 must surface as an error")
	}
}
