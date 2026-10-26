package mail

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

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
	Truncated   bool
}

type partData struct {
	att  Attachment
	data []byte
}

func validText(data []byte, truncated *bool) string {
	if utf8.Valid(data) {
		return string(data)
	}
	*truncated = true
	return strings.ToValidUTF8(string(data), "�")
}

func parseAll(raw []byte) ([]partData, string, string, bool, error) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		truncated := true
		return nil, validText(raw, &truncated), "", true, nil
	}
	var (
		parts     []partData
		text      string
		html      string
		idx       int
		truncated bool
	)
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if p == nil {
			truncated = true
			break
		}
		if err != nil {
			truncated = true
		}
		idx++
		data, readErr := io.ReadAll(p.Body)
		if readErr != nil {
			truncated = true
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			mt, _, _ := h.ContentType()
			if mt == "text/html" && html == "" {
				if readErr == nil {
					html = validText(data, &truncated)
				}
			} else if mt == "text/plain" && text == "" {
				if readErr == nil {
					text = validText(data, &truncated)
				}
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
	if text == "" && html == "" && len(parts) == 0 {
		body := raw
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			body = raw[i+4:]
		} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
			body = raw[i+2:]
		} else {
			body = nil
		}
		if len(bytes.TrimSpace(body)) > 0 {
			truncated = true
			text = validText(raw, &truncated)
		}
	}
	return parts, text, html, truncated, nil
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
	parts, text, html, truncated, err := parseAll(raw)
	if err != nil {
		return nil, err
	}
	out := &ParsedBody{Text: text, HTML: html, Truncated: truncated}
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
	parts, _, _, _, err := parseAll(raw)
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

var snippetCutRe = regexp.MustCompile(`^(On .{1,200} wrote:|.{0,100}写道[:：]|[-_]{2,}\s*(Original Message|原始邮件|转发邮件|Forwarded message)|Begin forwarded message[:：]|_{10,}|-{10,}\s*$)`)

var snippetFromRe = regexp.MustCompile(`^(From|发件人)[:：]`)

var snippetHdrRe = regexp.MustCompile(`^(Sent|Date|To|Cc|Subject|发送时间|日期|收件人|抄送|主题)[:：]`)

var snippetGreetRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(hi|hello|hey|hiya|howdy|greetings|good\s+(morning|afternoon|evening|day))\b[\s,!.]*([a-z][a-z'.-]*( [a-z][a-z'.-]*){0,1})?[\s,!.]*$`),
	regexp.MustCompile(`(?i)^dear\s+[^,.!?;]{1,40}[,:]$`),
	regexp.MustCompile(`^[^，。！？；、]{0,24}(您好|你好)[，,：:!！]?\s*$`),
	regexp.MustCompile(`^(大家好|您好|你好)[，,：:!！]?\s*$`),
}

const SnippetVersion = 1

func MakeSnippet(text, htmlBody string) string {
	if s := snippetClean(text); s != "" {
		return s
	}
	if strings.TrimSpace(htmlBody) != "" {
		return snippetClean(htmlToText(htmlBody))
	}
	return ""
}

func snippetClean(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	lines := strings.Split(src, "\n")
	var kept []string
	for i, ln := range lines {
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
		if strings.HasPrefix(t, "On ") && !strings.Contains(t, "wrote:") && i+1 < len(lines) && strings.HasSuffix(strings.TrimSpace(lines[i+1]), "wrote:") {
			break
		}
		if strings.Contains(t, "@") && snippetFromRe.MatchString(t) && snippetHeaderFollows(lines, i) {
			break
		}
		kept = append(kept, t)
	}
	start := 0
	for start < len(kept) && (kept[start] == "" || snippetIsGreeting(kept[start])) {
		start++
	}
	if start > 0 && start < len(kept) {
		kept = kept[start:]
	}
	s := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	return truncateSnippet(s)
}

func snippetHeaderFollows(lines []string, i int) bool {
	for j := i + 1; j < len(lines) && j <= i+4; j++ {
		if snippetHdrRe.MatchString(strings.TrimSpace(lines[j])) {
			return true
		}
	}
	return false
}

func snippetIsGreeting(line string) bool {
	for _, re := range snippetGreetRes {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func truncateSnippet(s string) string {
	r := []rune(s)
	if len(r) <= snippetMaxRunes {
		return s
	}
	cut := snippetMaxRunes - 1
	idx := -1
	for i := 0; i < cut; i++ {
		if r[i] == ' ' {
			idx = i
		}
	}
	if idx >= cut*2/3 {
		return string(r[:idx]) + "…"
	}
	return string(r[:cut]) + "…"
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
			case "script", "style", "head", "title", "noscript":
				return
			case "br", "p", "div", "tr", "td", "li", "blockquote", "table",
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
