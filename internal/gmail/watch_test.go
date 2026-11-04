package gmail

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type watchRoundTripper func(*http.Request) (*http.Response, error)

func (f watchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWatchPostsTopicAndParsesExpiration(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	c := NewClient(Creds{RefreshToken: "refresh"})
	c.http = &http.Client{Transport: watchRoundTripper(func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotPath = req.URL.Path
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"historyId":"777","expiration":"1893456000000"}`)),
		}, nil
	})}
	c.access = "token"
	c.accessExp = time.Now().Add(time.Hour)
	res, err := c.Watch(context.Background(), "projects/p/topics/gmail")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/watch") {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"topicName":"projects/p/topics/gmail"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if res.HistoryID != "777" || res.Expiration != 1893456000000 {
		t.Fatalf("result = %+v", res)
	}
}
