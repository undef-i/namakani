package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type GoogleEngine struct {
	BaseURL string
	HTTP    *http.Client
}

func NewGoogleEngine() *GoogleEngine {
	return &GoogleEngine{
		BaseURL: "https://clients5.google.com/translate_a/t",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *GoogleEngine) Key() string { return "google" }

func googleLang(l string) string {
	switch l {
	case "zh", "zh-Hans":
		return "zh-CN"
	case "zh-Hant":
		return "zh-TW"
	}
	return l
}

func (g *GoogleEngine) Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	if len(segs) == 0 {
		return nil, nil
	}
	if src == "" {
		src = "auto"
	}
	q := url.Values{}
	q.Set("client", "dict-chrome-ex")
	q.Set("sl", googleLang(src))
	q.Set("tl", googleLang(tgt))
	for _, s := range segs {
		q.Add("q", s.Text)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google: HTTP %d", resp.StatusCode)
	}
	var results []string
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil, fmt.Errorf("google: bad response: %w", err)
	}
	if len(results) != len(segs) {
		return nil, fmt.Errorf("google: result count %d for %d segments", len(results), len(segs))
	}
	out := make([]Segment, len(segs))
	for i, s := range segs {
		out[i] = Segment{ID: s.ID, Text: results[i]}
	}
	return out, nil
}
