package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type LLMEngine struct {
	Format  string
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

func NewLLMEngine(format, baseURL, model, apiKey string) *LLMEngine {
	return &LLMEngine{
		Format:  format,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (e *LLMEngine) Key() string {
	return "llm:" + e.Format + ":" + e.Model
}

const llmSystemPrompt = "You are a translation engine. The user message is a JSON array of segments, each an object with \"id\" and \"text\". Translate every segment from the source language into the target language. Return only a JSON array of objects with the same \"id\" values and the translated \"text\", in the same order, with no other output. A segment may contain placeholders such as {1}, {/1} or {2/}: copy every placeholder into the translation exactly as many times as it appears in the source segment. Never translate, rename or drop a placeholder."

func llmJoinURL(base, leaf, full string) string {
	if strings.HasSuffix(base, full) {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + leaf
	}
	return base + "/v1" + leaf
}

func (e *LLMEngine) endpoint() string {
	if e.Format == "anthropic" {
		return llmJoinURL(e.BaseURL, "/messages", "/v1/messages")
	}
	return llmJoinURL(e.BaseURL, "/chat/completions", "/v1/chat/completions")
}

func (e *LLMEngine) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmJoinURL(e.BaseURL, "/models", "/v1/models"), nil)
	if err != nil {
		return nil, err
	}
	if e.Format == "anthropic" {
		if e.APIKey != "" {
			req.Header.Set("X-Api-Key", e.APIKey)
		}
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, snip)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: bad models response: %w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (e *LLMEngine) Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	if len(segs) == 0 {
		return nil, nil
	}
	userJSON, err := json.Marshal(segs)
	if err != nil {
		return nil, err
	}
	sys := llmSystemPrompt + "\nSource language: " + src + "\nTarget language: " + tgt
	var payload map[string]any
	if e.Format == "anthropic" {
		payload = map[string]any{
			"model":      e.Model,
			"max_tokens": 8192,
			"system":     sys,
			"messages": []map[string]string{
				{"role": "user", "content": string(userJSON)},
			},
		}
	} else {
		payload = map[string]any{
			"model":       e.Model,
			"temperature": 0,
			"messages": []map[string]string{
				{"role": "system", "content": sys},
				{"role": "user", "content": string(userJSON)},
			},
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Format == "anthropic" {
		if e.APIKey != "" {
			req.Header.Set("X-Api-Key", e.APIKey)
		}
		req.Header.Set("Anthropic-Version", "2023-06-01")
	} else if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(body)
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, snip)
	}
	var content string
	if e.Format == "anthropic" {
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("llm: bad response: %w", err)
		}
		for _, c := range parsed.Content {
			if c.Type == "text" {
				content = c.Text
				break
			}
		}
	} else {
		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("llm: bad response: %w", err)
		}
		if len(parsed.Choices) > 0 {
			content = parsed.Choices[0].Message.Content
		}
	}
	if content == "" {
		return nil, fmt.Errorf("llm: empty completion")
	}
	return parseSegmentReply(content, segs)
}

func parseSegmentReply(content string, want []Segment) ([]Segment, error) {
	txt := strings.TrimSpace(content)
	if strings.HasPrefix(txt, "```") {
		if nl := strings.Index(txt, "\n"); nl >= 0 {
			txt = txt[nl+1:]
		}
		txt = strings.TrimSuffix(strings.TrimSpace(txt), "```")
		txt = strings.TrimSpace(txt)
	}
	lo := strings.Index(txt, "[")
	hi := strings.LastIndex(txt, "]")
	if lo < 0 || hi <= lo {
		return nil, fmt.Errorf("llm: no segment array in completion")
	}
	var out []Segment
	if err := json.Unmarshal([]byte(txt[lo:hi+1]), &out); err != nil {
		return nil, fmt.Errorf("llm: bad segment array: %w", err)
	}
	wantIDs := make(map[int]bool, len(want))
	for _, s := range want {
		wantIDs[s.ID] = true
	}
	if len(out) != len(want) {
		return nil, fmt.Errorf("llm: returned %d segments for %d requested", len(out), len(want))
	}
	seen := make(map[int]bool, len(out))
	for _, s := range out {
		if !wantIDs[s.ID] || seen[s.ID] {
			return nil, fmt.Errorf("llm: returned segment ids do not match the request")
		}
		seen[s.ID] = true
	}
	return out, nil
}
