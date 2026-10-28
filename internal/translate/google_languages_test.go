package translate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const fixtureLanguagesEn = `<!DOCTYPE html><html><body>
<table><tr><td>Decoy</td><td><code>xx</code></td></tr></table>
<h2 data-text="Neural Machine Translation model" id="nmt" tabindex="-1">Neural Machine Translation model</h2>
<table>
<thead><tr><th>Language</th><th>ISO-639 code</th></tr></thead>
<tbody>
<tr><td>Abkhaz</td><td><code translate="no" dir="ltr">ab</code></td></tr>
<tr><td>Chinese (Simplified)</td><td><code translate="no" dir="ltr">zh-CN</code> or <code translate="no" dir="ltr">zh</code> (<a href="https://tools.ietf.org/html/bcp47">BCP-47</a>)</td></tr>
<tr><td>Filipino (Tagalog)</td><td><code translate="no" dir="ltr">fil</code> or <code translate="no" dir="ltr">tl</code></td></tr>
<tr><td>Konkani</td><td><code translate="no" dir="ltr">gom</code></td></tr>
</tbody>
</table>
</body></html>`

const fixtureLanguagesZh = `<!DOCTYPE html><html><body>
<table><tr><td>诱饵</td><td><code>xx</code></td></tr></table>
<h2 data-text="神经机器翻译模型" id="nmt" tabindex="-1">神经机器翻译模型</h2>
<table>
<thead><tr><th>语言</th><th>ISO-639 代码</th></tr></thead>
<tbody>
<tr><td>阿布哈兹语</td><td><code dir="ltr" translate="no">ab</code></td></tr>
<tr><td>简体中文</td><td><code dir="ltr" translate="no">zh-CN</code> 或 <code dir="ltr" translate="no">zh</code> (BCP-47)</td></tr>
<tr><td>菲律宾语（塔加拉语）</td><td><code dir="ltr" translate="no">fil</code> 或 <code dir="ltr" translate="no">tl</code></td></tr>
<tr><td>贡根语</td><td><code dir="ltr" translate="no">gom</code></td></tr>
</tbody>
</table>
</body></html>`

func TestParseGoogleLanguagesAlignsCodesAndNames(t *testing.T) {
	got, err := ParseGoogleLanguages([]byte(fixtureLanguagesEn), []byte(fixtureLanguagesZh))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Language{
		{Code: "ab", En: "Abkhaz", Zh: "阿布哈兹语"},
		{Code: "zh-CN", En: "Chinese (Simplified)", Zh: "简体中文"},
		{Code: "fil", En: "Filipino (Tagalog)", Zh: "菲律宾语（塔加拉语）"},
		{Code: "gom", En: "Konkani", Zh: "贡根语"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("languages = %+v, want %+v", got, want)
	}
}

func TestParseGoogleLanguagesCodeMismatchIsError(t *testing.T) {
	bad := strings.Replace(fixtureLanguagesZh, ">gom<", ">xyz<", 1)
	if _, err := ParseGoogleLanguages([]byte(fixtureLanguagesEn), []byte(bad)); err == nil {
		t.Fatal("a zh-CN edition whose codes do not line up with the en edition must be an error")
	}
}

func TestParseGoogleLanguagesMissingTableIsError(t *testing.T) {
	if _, err := ParseGoogleLanguages([]byte(`<html><body><p>none</p></body></html>`), []byte(fixtureLanguagesZh)); err == nil {
		t.Fatal("a page without the NMT table must be an error")
	}
}

func TestFetchGoogleLanguagesUsesHlEditions(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("hl"))
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Query().Get("hl") == "zh-CN" {
			_, _ = io.WriteString(w, fixtureLanguagesZh)
		} else {
			_, _ = io.WriteString(w, fixtureLanguagesEn)
		}
	}))
	defer srv.Close()
	got, err := FetchGoogleLanguages(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 4 || got[1].Code != "zh-CN" || got[1].Zh != "简体中文" {
		t.Fatalf("languages = %+v", got)
	}
	if len(seen) != 2 || seen[0] != "en" || seen[1] != "zh-CN" {
		t.Fatalf("hl requests = %v, want [en zh-CN]", seen)
	}
}
