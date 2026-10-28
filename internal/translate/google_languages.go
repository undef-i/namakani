package translate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/html"
)

type Language struct {
	Code string `json:"code"`
	En   string `json:"en"`
	Zh   string `json:"zh"`
}

const GoogleLanguagesURL = "https://cloud.google.com/translate/docs/languages"

func FetchGoogleLanguages(ctx context.Context, client *http.Client, baseURL string) ([]Language, error) {
	if client == nil {
		client = http.DefaultClient
	}
	en, err := fetchLanguagesPage(ctx, client, baseURL+"?hl=en")
	if err != nil {
		return nil, err
	}
	zh, err := fetchLanguagesPage(ctx, client, baseURL+"?hl=zh-CN")
	if err != nil {
		return nil, err
	}
	return ParseGoogleLanguages(en, zh)
}

func fetchLanguagesPage(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google languages: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// ParseGoogleLanguages reads the NMT table (the first table after the
// h2#nmt heading) of the en and zh-CN editions of the supported-languages
// page. A row may print two codes; the first is the offered code.
func ParseGoogleLanguages(enPage, zhPage []byte) ([]Language, error) {
	en, err := parseNmtRows(enPage)
	if err != nil {
		return nil, fmt.Errorf("en edition: %w", err)
	}
	zh, err := parseNmtRows(zhPage)
	if err != nil {
		return nil, fmt.Errorf("zh-CN edition: %w", err)
	}
	if len(en) != len(zh) {
		return nil, fmt.Errorf("google languages: en has %d rows, zh-CN has %d", len(en), len(zh))
	}
	out := make([]Language, 0, len(en))
	for i := range en {
		ec, zc := en[i].codes, zh[i].codes
		if len(ec) != len(zc) {
			return nil, fmt.Errorf("google languages: row %d code count differs: %v vs %v", i, ec, zc)
		}
		for j := range ec {
			if ec[j] != zc[j] {
				return nil, fmt.Errorf("google languages: row %d code differs: %v vs %v", i, ec, zc)
			}
		}
		if en[i].name == "" || zh[i].name == "" {
			return nil, fmt.Errorf("google languages: row %d (%s) has an empty name", i, ec[0])
		}
		out = append(out, Language{Code: ec[0], En: en[i].name, Zh: zh[i].name})
	}
	return out, nil
}

type nmtRow struct {
	name  string
	codes []string
}

func parseNmtRows(page []byte) ([]nmtRow, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	table := nmtTable(doc)
	if table == nil {
		return nil, fmt.Errorf("google languages: NMT table not found")
	}
	var rows []nmtRow
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			if row, ok := parseNmtRow(n); ok {
				rows = append(rows, row)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(table)
	if len(rows) == 0 {
		return nil, fmt.Errorf("google languages: NMT table has no language rows")
	}
	return rows, nil
}

func nmtTable(doc *html.Node) *html.Node {
	seen := false
	var found *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode {
			if !seen {
				for _, a := range n.Attr {
					if a.Key == "id" && a.Val == "nmt" {
						seen = true
						break
					}
				}
			} else if n.Data == "table" {
				found = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
			if found != nil {
				return
			}
		}
	}
	walk(doc)
	return found
}

func parseNmtRow(tr *html.Node) (nmtRow, bool) {
	var cells []*html.Node
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "td" {
			cells = append(cells, c)
		}
	}
	if len(cells) < 2 {
		return nmtRow{}, false
	}
	var codes []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "code" {
			if code := strings.TrimSpace(nodeText(n)); code != "" {
				codes = append(codes, code)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(cells[1])
	if len(codes) == 0 {
		return nmtRow{}, false
	}
	return nmtRow{name: strings.Join(strings.Fields(nodeText(cells[0])), " "), codes: codes}, true
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(x *html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
