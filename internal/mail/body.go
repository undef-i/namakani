package mail

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

type Attachment struct {
	PartPath  string
	Filename  string
	Mime      string
	Size      int64
	ContentID string
	Inline    bool
	Bytes     []byte
}

type ParsedBody struct {
	Text        string
	HTML        string
	Attachments []Attachment
	References  []string
	InReplyTo   string
}

type partData struct {
	att  Attachment
	data []byte
}

func parseAll(raw []byte) ([]partData, string, string, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, "", "", err
	}
	var (
		parts []partData
		text  string
		html  string
		idx   int
	)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		idx++
		data, err := io.ReadAll(p.Body)
		if err != nil {
			return nil, "", "", err
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			mt, _, _ := h.ContentType()
			if mt == "text/html" && html == "" {
				html = string(data)
			} else if mt == "text/plain" && text == "" {
				text = string(data)
			} else {
				parts = append(parts, partData{
					att: Attachment{
						PartPath:  strconv.Itoa(idx),
						Mime:      mt,
						Size:      int64(len(data)),
						ContentID: strings.Trim(h.Get("Content-Id"), "<>"),
						Inline:    true,
					},
					data: data,
				})
			}
		case *mail.AttachmentHeader:
			mt, _, _ := h.ContentType()
			fn, _ := h.Filename()
			if fn == "" {
				fn = fallbackName(mt)
			}
			parts = append(parts, partData{
				att: Attachment{
					PartPath:  strconv.Itoa(idx),
					Filename:  fn,
					Mime:      mt,
					Size:      int64(len(data)),
					ContentID: strings.Trim(h.Get("Content-Id"), "<>"),
				},
				data: data,
			})
		default:
			parts = append(parts, partData{
				att: Attachment{
					PartPath: strconv.Itoa(idx),
					Filename: fallbackName("application/octet-stream"),
					Mime:     "application/octet-stream",
					Size:     int64(len(data)),
				},
				data: data,
			})
		}
	}
	return parts, text, html, nil
}

func fallbackName(mime string) string {
	switch mime {
	case "message/rfc822":
		return "message.eml"
	case "application/pdf":
		return "document.pdf"
	default:
		if strings.HasPrefix(mime, "image/") {
			return "image." + strings.TrimPrefix(mime, "image/")
		}
		return "attachment.bin"
	}
}

func ParseBody(raw []byte) (*ParsedBody, error) {
	parts, text, html, err := parseAll(raw)
	if err != nil {
		return nil, err
	}
	out := &ParsedBody{Text: text, HTML: html}
	for _, p := range parts {
		out.Attachments = append(out.Attachments, p.att)
	}
	if mr, err := mail.CreateReader(bytes.NewReader(raw)); err == nil {
		if refs, err := mr.Header.MsgIDList("References"); err == nil {
			for _, r := range refs {
				out.References = append(out.References, string(r))
			}
		}
		if ids, err := mr.Header.MsgIDList("In-Reply-To"); err == nil && len(ids) > 0 {
			out.InReplyTo = string(ids[len(ids)-1])
		} else {
			out.InReplyTo = strings.Trim(mr.Header.Get("In-Reply-To"), "<> \t")
		}
	}
	return out, nil
}

func ParseBodyPart(raw []byte, partPath string) (*Attachment, error) {
	parts, _, _, err := parseAll(raw)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p.att.PartPath == partPath {
			a := p.att
			a.Bytes = p.data
			return &a, nil
		}
	}
	return nil, fmt.Errorf("part %s not found", partPath)
}

const snippetMaxRunes = 140

var snippetCutRe = regexp.MustCompile(`^(On .{1,160} wrote:|.{0,100}写道[:：]|[-_]{3,}\s*(Original Message|原始邮件|转发邮件)|_{10,}|-{10,}\s*$)`)

func MakeSnippet(text, htmlBody string) string {
	src := text
	if strings.TrimSpace(src) == "" && htmlBody != "" {
		src = htmlToText(htmlBody)
	}
	if strings.TrimSpace(src) == "" {
		return ""
	}
	var kept []string
	for _, ln := range strings.Split(src, "\n") {
		t := strings.TrimSpace(ln)
		if t == "--" {
			break
		}
		if strings.HasPrefix(t, ">") {
			continue
		}
		if snippetCutRe.MatchString(t) {
			break
		}
		kept = append(kept, ln)
	}
	s := strings.Join(strings.Fields(strings.Join(kept, "\n")), " ")
	r := []rune(s)
	if len(r) > snippetMaxRunes {
		s = string(r[:snippetMaxRunes])
	}
	return s
}

func htmlToText(htmlBody string) string {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "title":
				return
			case "br", "p", "div", "tr", "li", "blockquote", "table",
				"h1", "h2", "h3", "h4", "h5", "h6":
				sb.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return sb.String()
}
