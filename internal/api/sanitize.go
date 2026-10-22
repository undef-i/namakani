package api

import (
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

var mailPolicy = buildMailPolicy()

func buildMailPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowDataURIImages()
	p.AllowAttrs("style").Globally()
	p.AllowAttrs("background", "bgcolor", "align", "valign", "width", "height",
		"border", "cellpadding", "cellspacing").
		OnElements("table", "thead", "tbody", "tr", "td", "th", "img")
	p.AllowElements("center", "font")
	p.AllowAttrs("color", "face", "size").OnElements("font")
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}

func sanitizeMailHTML(html string, cidURLs map[string]string) string {
	for cid, url := range cidURLs {
		html = strings.ReplaceAll(html, "cid:"+cid, url)
	}
	return mailPolicy.Sanitize(html)
}
