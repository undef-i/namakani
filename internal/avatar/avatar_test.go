package avatar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestParseBIMILogoURL(t *testing.T) {
	cases := []struct {
		name string
		txts []string
		want string
		ok   bool
	}{
		{"standard", []string{"v=BIMI1; l=https://example.com/logo.svg; a=https://example.com/vmc.pem"}, "https://example.com/logo.svg", true},
		{"among others", []string{"v=spf1 -all", "v=BIMI1; l=https://brand.example/bimi.svg"}, "https://brand.example/bimi.svg", true},
		{"empty location means none", []string{"v=BIMI1; l=;"}, "", false},
		{"no bimi record", []string{"v=spf1 include:_spf.example.com -all"}, "", false},
		{"not https", []string{"v=BIMI1; l=http://example.com/logo.svg"}, "", false},
		{"no records", nil, "", false},
	}
	for _, c := range cases {
		got, ok := ParseBIMILogoURL(c.txts)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: ParseBIMILogoURL = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestSanitizeSVGStripsScriptsAndExternalRefs(t *testing.T) {
	in := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" onload="alert(1)">
<script>alert(2)</script>
<image href="https://evil.example/x.png" width="100" height="100"/>
<a href="https://evil.example/"><rect x="0" y="0" width="100" height="100" fill="#ffffff"/></a>
<path d="M10 10 H 90 V 90 H 10 Z" fill="#123456"/>
<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject>
</svg>`
	out, ok := SanitizeSVG([]byte(in))
	if !ok {
		t.Fatal("a logo with a real path should survive sanitizing")
	}
	s := string(out)
	for _, bad := range []string{"script", "evil.example", "onload", "foreignObject", "alert"} {
		if strings.Contains(s, bad) {
			t.Fatalf("sanitized svg still contains %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, "<path") || !strings.Contains(s, "#123456") {
		t.Fatalf("legitimate drawing content was lost: %s", s)
	}
	if !strings.Contains(s, "<svg") || !strings.Contains(s, `viewBox="0 0 100 100"`) {
		t.Fatalf("svg root or viewBox lost: %s", s)
	}
}

func TestSanitizeSVGRejectsNonSVG(t *testing.T) {
	if _, ok := SanitizeSVG([]byte(`<html><body>not a logo</body></html>`)); ok {
		t.Fatal("non-svg content must be rejected")
	}
	if _, ok := SanitizeSVG([]byte(`garbage`)); ok {
		t.Fatal("garbage must be rejected")
	}
	if _, ok := SanitizeSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`)); ok {
		t.Fatal("an svg with no drawable content must be rejected")
	}
}

func TestGravatarHashNormalization(t *testing.T) {
	sum := sha256.Sum256([]byte("noreply@example.com"))
	want := hex.EncodeToString(sum[:])
	if got := GravatarHash("  NoReply@Example.COM "); got != want {
		t.Fatalf("GravatarHash = %q, want sha256 of the trimmed lowercase address %q", got, want)
	}
}

type fakeNet struct {
	txts     map[string][]string
	pages    map[string]fakePage
	txtCalls []string
	getCalls []string
}

type fakePage struct {
	status int
	mime   string
	body   []byte
}

func (f *fakeNet) lookupTXT(ctx context.Context, name string) ([]string, error) {
	f.txtCalls = append(f.txtCalls, name)
	if v, ok := f.txts[name]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

func (f *fakeNet) fetch(ctx context.Context, url string) (int, string, []byte, error) {
	f.getCalls = append(f.getCalls, url)
	if p, ok := f.pages[url]; ok {
		return p.status, p.mime, p.body, nil
	}
	return 404, "", nil, nil
}

func testResolver(net *fakeNet) *Resolver {
	return &Resolver{LookupTXT: net.lookupTXT, Fetch: net.fetch}
}

func TestResolvePrefersBIMI(t *testing.T) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#000000"/></svg>`
	net := &fakeNet{
		txts: map[string][]string{
			"default._bimi.example.com": {"v=BIMI1; l=https://example.com/logo.svg"},
		},
		pages: map[string]fakePage{
			"https://example.com/logo.svg": {200, "image/svg+xml", []byte(svg)},
		},
	}
	r := testResolver(net)
	kind, mime, data := r.Resolve(context.Background(), "News@Example.com")
	if kind != "bimi" || mime != "image/svg+xml" || !strings.Contains(string(data), "<rect") {
		t.Fatalf("resolve = %q %q %q", kind, mime, data)
	}
	if len(net.txtCalls) != 1 || net.txtCalls[0] != "default._bimi.example.com" {
		t.Fatalf("bimi lookup went to %v", net.txtCalls)
	}
	for _, u := range net.getCalls {
		if strings.Contains(u, "gravatar.com") {
			t.Fatal("gravatar must not be consulted when BIMI yields a logo")
		}
	}
}

func TestResolveFallsToGravatarWhenNoBIMI(t *testing.T) {
	net := &fakeNet{
		txts: map[string][]string{},
		pages: map[string]fakePage{
			"https://www.gravatar.com/avatar/" + GravatarHash("someone@example.org") + "?d=404&s=128": {200, "image/png", []byte("PNGBYTES")},
		},
	}
	r := testResolver(net)
	kind, mime, data := r.Resolve(context.Background(), "someone@example.org")
	if kind != "gravatar" || mime != "image/png" || string(data) != "PNGBYTES" {
		t.Fatalf("resolve = %q %q %q", kind, mime, data)
	}
}

func TestResolveGravatar404MeansNoImage(t *testing.T) {
	net := &fakeNet{txts: map[string][]string{}, pages: map[string]fakePage{}}
	r := testResolver(net)
	kind, _, data := r.Resolve(context.Background(), "ghost@example.org")
	if kind != "" || len(data) != 0 {
		t.Fatalf("gravatar 404 must mean no image, got kind %q", kind)
	}
	called := false
	for _, u := range net.getCalls {
		if strings.Contains(u, "d=404") {
			called = true
		}
	}
	if !called {
		t.Fatalf("gravatar was not queried with d=404: %v", net.getCalls)
	}
}

func TestResolveBadBIMISVGFallsToGravatar(t *testing.T) {
	net := &fakeNet{
		txts: map[string][]string{
			"default._bimi.example.net": {"v=BIMI1; l=https://example.net/logo.svg"},
		},
		pages: map[string]fakePage{
			"https://example.net/logo.svg": {200, "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
			"https://www.gravatar.com/avatar/" + GravatarHash("a@example.net") + "?d=404&s=128": {200, "image/jpeg", []byte("JPGBYTES")},
		},
	}
	r := testResolver(net)
	kind, _, data := r.Resolve(context.Background(), "a@example.net")
	if kind != "gravatar" || string(data) != "JPGBYTES" {
		t.Fatalf("an unusable BIMI svg must fall through to gravatar, got kind %q", kind)
	}
}
