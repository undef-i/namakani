package translate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capturedRequest struct {
	path   string
	header http.Header
	body   map[string]any
}

func llmStubServer(t *testing.T, capReq *capturedRequest, reply string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capReq.path = r.URL.Path
		capReq.header = r.Header.Clone()
		_ = json.Unmarshal(raw, &capReq.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
}

func TestLLMOpenAIShapeAndParse(t *testing.T) {
	var capReq capturedRequest
	reply := `{"choices":[{"message":{"role":"assistant","content":"[{\"id\":3,\"text\":\"译文甲\"},{\"id\":5,\"text\":\"译文乙\"}]"}}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "model-x", "secret-key")
	out, err := eng.Translate(context.Background(), []Segment{
		{ID: 3, Text: "First {1}one{/1}."},
		{ID: 5, Text: "Second one."},
	}, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 || out[0].Text != "译文甲" || out[1].Text != "译文乙" {
		t.Fatalf("bad parse: %+v", out)
	}
	if capReq.path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", capReq.path)
	}
	if got := capReq.header.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("authorization = %q", got)
	}
	msgs, ok := capReq.body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages shape wrong: %v", capReq.body["messages"])
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("first message role = %v", sys["role"])
	}
	usr := msgs[1].(map[string]any)
	content, _ := usr["content"].(string)
	if !strings.Contains(content, `"id":3`) || !strings.Contains(content, "First {1}one{/1}.") {
		t.Fatalf("user content does not carry the segment protocol: %q", content)
	}
}

func TestLLMAnthropicShapeAndParse(t *testing.T) {
	var capReq capturedRequest
	reply := `{"content":[{"type":"text","text":"[{\"id\":2,\"text\":\"译文\"}]"}],"stop_reason":"end_turn"}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("anthropic", srv.URL+"/v1", "claude-x", "anthro-key")
	out, err := eng.Translate(context.Background(), []Segment{{ID: 2, Text: "Hello."}}, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 1 || out[0].Text != "译文" {
		t.Fatalf("bad parse: %+v", out)
	}
	if capReq.path != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", capReq.path)
	}
	if got := capReq.header.Get("X-Api-Key"); got != "anthro-key" {
		t.Fatalf("x-api-key = %q", got)
	}
	if got := capReq.header.Get("Anthropic-Version"); got == "" {
		t.Fatal("anthropic-version header missing")
	}
	if _, ok := capReq.body["system"].(string); !ok {
		t.Fatalf("system must be a top-level string: %v", capReq.body["system"])
	}
	msgs, ok := capReq.body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages shape wrong: %v", capReq.body["messages"])
	}
}

func TestLLMResponseIDMismatchIsError(t *testing.T) {
	var capReq capturedRequest
	reply := `{"choices":[{"message":{"role":"assistant","content":"[{\"id\":99,\"text\":\"错位\"}]"}}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "model-x", "k")
	_, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if err == nil {
		t.Fatal("a response whose ids do not match the request must be an error")
	}
}

func TestLLMResponseInCodeFenceParses(t *testing.T) {
	var capReq capturedRequest
	reply := "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n[{\\\"id\\\":1,\\\"text\\\":\\\"译文\\\"}]\\n```\"}}]}"
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "model-x", "k")
	out, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 1 || out[0].Text != "译文" {
		t.Fatalf("bad parse: %+v", out)
	}
}

func TestLLMOpenAIJSONModeAndTemperature(t *testing.T) {
	var capReq capturedRequest
	reply := `{"choices":[{"message":{"role":"assistant","content":"[{\"id\":1,\"text\":\"译文\"}]"},"finish_reason":"stop"}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "model-x", "k")
	if _, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh"); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := capReq.body["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want 0", got)
	}
	rf, ok := capReq.body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Fatalf("response_format = %v, want json_object mode", capReq.body["response_format"])
	}
}

func TestLLMChatLengthFinishIsTruncatedError(t *testing.T) {
	var capReq capturedRequest
	reply := `{"choices":[{"message":{"role":"assistant","content":"[{\"id\":1,\"text\":\"译了一半\"}]"},"finish_reason":"length"}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("openai", srv.URL, "model-x", "k")
	_, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestLLMAnthropicTemperatureAndTruncation(t *testing.T) {
	var capReq capturedRequest
	reply := `{"content":[{"type":"text","text":"[{\"id\":1,\"text\":\"译文\"}]"}],"stop_reason":"end_turn"}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("anthropic", srv.URL, "claude-x", "k")
	if _, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh"); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := capReq.body["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want 0", got)
	}

	var capReqT capturedRequest
	replyT := `{"content":[{"type":"text","text":"[{\"id\":1,\"text\":\"译了一半"}],"stop_reason":"max_tokens"}`
	srvT := llmStubServer(t, &capReqT, replyT)
	defer srvT.Close()
	engT := NewLLMEngine("anthropic", srvT.URL, "claude-x", "k")
	_, err := engT.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestLLMResponsesShapeAndParse(t *testing.T) {
	var capReq capturedRequest
	reply := `{"id":"resp_1","status":"completed","incomplete_details":null,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"id\":3,\"text\":\"译文甲\"},{\"id\":5,\"text\":\"译文乙\"}]"}]}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("responses", srv.URL, "model-x", "secret-key")
	out, err := eng.Translate(context.Background(), []Segment{
		{ID: 3, Text: "First {1}one{/1}."},
		{ID: 5, Text: "Second one."},
	}, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 || out[0].Text != "译文甲" || out[1].Text != "译文乙" {
		t.Fatalf("bad parse: %+v", out)
	}
	if capReq.path != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", capReq.path)
	}
	if got := capReq.header.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("authorization = %q", got)
	}
	want := llmSystemPrompt + "\nTarget language: zh"
	if got := capReq.body["instructions"]; got != want {
		t.Fatalf("instructions = %q, want the system prompt with only the target language", got)
	}
	if got := capReq.body["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want 0", got)
	}
	tf, ok := capReq.body["text"].(map[string]any)
	if !ok {
		t.Fatalf("text config missing: %v", capReq.body)
	}
	format, ok := tf["format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Fatalf("text.format = %v, want json_object mode", tf["format"])
	}
	input, ok := capReq.body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input shape wrong: %v", capReq.body["input"])
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("input role = %v", msg["role"])
	}
	content, _ := msg["content"].(string)
	if !strings.Contains(content, `"id":3`) || !strings.Contains(content, "First {1}one{/1}.") {
		t.Fatalf("input content does not carry the segment protocol: %q", content)
	}
}

func TestLLMResponsesTruncationIsError(t *testing.T) {
	var capReq capturedRequest
	reply := `{"id":"resp_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"id\":1,\"text\":\"译了一半"}]}]}`
	srv := llmStubServer(t, &capReq, reply)
	defer srv.Close()
	eng := NewLLMEngine("responses", srv.URL, "model-x", "k")
	_, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestLLMSystemPromptCarriesOnlyTargetLanguage(t *testing.T) {
	reply := `{"choices":[{"message":{"role":"assistant","content":"[{\"id\":1,\"text\":\"译文\"}]"}}]}`
	want := llmSystemPrompt + "\nTarget language: zh"

	var capReq capturedRequest
	srv := llmStubServer(t, &capReq, reply)
	eng := NewLLMEngine("openai", srv.URL, "model-x", "k")
	if _, err := eng.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh"); err != nil {
		t.Fatalf("translate: %v", err)
	}
	srv.Close()
	msgs, ok := capReq.body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages shape wrong: %v", capReq.body["messages"])
	}
	if got := msgs[0].(map[string]any)["content"]; got != want {
		t.Fatalf("openai system = %q, want %q", got, want)
	}

	var capReqA capturedRequest
	replyA := `{"content":[{"type":"text","text":"[{\"id\":1,\"text\":\"译文\"}]"}],"stop_reason":"end_turn"}`
	srvA := llmStubServer(t, &capReqA, replyA)
	defer srvA.Close()
	engA := NewLLMEngine("anthropic", srvA.URL, "claude-x", "k")
	if _, err := engA.Translate(context.Background(), []Segment{{ID: 1, Text: "One."}}, "en", "zh"); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := capReqA.body["system"]; got != want {
		t.Fatalf("anthropic system = %q, want %q", got, want)
	}
}
