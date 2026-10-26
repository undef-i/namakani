package store

import (
	"strings"

	"golang.org/x/net/html"
)

func HTMLToText(htmlBody string) string {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "noscript") {
			return
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.Join(strings.Fields(sb.String()), " ")
}
