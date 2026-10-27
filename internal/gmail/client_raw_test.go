package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Gmail answers format=raw with standard padded base64url, not the
// unpadded form. Serve exactly that shape and require GetRaw to
// return the original MIME bytes for each padding class.
func TestGetRawDecodesPaddedForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"two padding", "Subject: a\r\n\r\nxy"},
		{"one padding", "Subject: a\r\n\r\nxyz"},
		{"no padding", "Subject: a\r\n\r\nx"},
	}
	for _, tc := range cases {
		enc := base64.URLEncoding.EncodeToString([]byte(tc.raw))
		pad := len(enc) - len(strings.TrimRight(enc, "="))
		wantPad := map[string]int{"two padding": 2, "one padding": 1, "no padding": 0}[tc.name]
		if pad != wantPad {
			t.Fatalf("%s: fixture encodes with %d padding chars, want %d", tc.name, pad, wantPad)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-1", "expires_in": 3600, "token_type": "Bearer"})
		})
		mux.HandleFunc("/gmail/v1/users/me/messages/m1", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") != "raw" {
				t.Errorf("format = %q, want raw", r.URL.Query().Get("format"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"raw": enc})
		})
		srv := httptest.NewServer(mux)
		oldAPI, oldToken := APIBase, TokenURL
		APIBase = srv.URL + "/gmail/v1/users/me"
		TokenURL = srv.URL + "/token"
		got, err := NewClient(testCreds()).GetRaw(context.Background(), "m1")
		APIBase, TokenURL = oldAPI, oldToken
		srv.Close()
		if err != nil {
			t.Fatalf("%s: GetRaw: %v", tc.name, err)
		}
		if string(got) != tc.raw {
			t.Fatalf("%s: decoded = %q, want %q", tc.name, got, tc.raw)
		}
	}
}
