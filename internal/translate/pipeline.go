package translate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

var skipTags = map[string]bool{
	"script": true, "style": true, "pre": true, "textarea": true,
	"noscript": true, "template": true, "head": true, "select": true,
	"svg": true, "math": true, "iframe": true, "object": true,
	"title": true, "button": true,
}

var inlineTags = map[string]bool{
	"a": true, "abbr": true, "b": true, "bdi": true, "bdo": true,
	"big": true, "cite": true, "data": true, "del": true, "dfn": true,
	"em": true, "font": true, "i": true, "ins": true, "mark": true,
	"q": true, "ruby": true, "s": true, "small": true, "span": true,
	"strong": true, "sub": true, "sup": true, "time": true, "tt": true,
	"u": true,
}

var protectedTags = map[string]bool{
	"br": true, "wbr": true, "hr": true, "img": true, "input": true,
	"code": true, "kbd": true, "samp": true, "var": true,
}

func isInlineElement(n *html.Node) bool {
	return n.Type == html.ElementNode && (inlineTags[n.Data] || protectedTags[n.Data])
}

func hasNoTranslate(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "translate" && a.Val == "no" {
			return true
		}
		if a.Key == "class" {
			for _, f := range strings.Fields(a.Val) {
				if f == "notranslate" {
					return true
				}
			}
		}
	}
	return false
}

func collectText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	if n.Type == html.ElementNode && protectedTags[n.Data] {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, sb)
	}
}

func runText(nodes []*html.Node) string {
	var sb strings.Builder
	for _, n := range nodes {
		collectText(n, &sb)
	}
	return sb.String()
}

type unit struct {
	parent *html.Node
	nodes  []*html.Node
	elems  map[int]*html.Node
	pieces []piece
	segIDs []int
}

type piece struct {
	text     string
	sentence bool
}

func splitSentences(s string) []piece {
	runes := []rune(s)
	var pieces []piece
	start := 0
	emit := func(end int) {
		if end > start {
			txt := string(runes[start:end])
			pieces = append(pieces, piece{text: txt, sentence: strings.TrimSpace(txt) != ""})
			start = end
		}
	}
	closers := map[rune]bool{
		'"': true, '\'': true, '”': true, '’': true, '）': true, ')': true,
		']': true, '」': true, '』': true, '》': true,
	}
	i := 0
	for i < len(runes) {
		r := runes[i]
		if r == '\n' || r == '\r' {
			j := i
			for j < len(runes) && (runes[j] == '\n' || runes[j] == '\r') {
				j++
			}
			emit(j)
			i = j
			continue
		}
		isCJKEnder := r == '。' || r == '！' || r == '？' || r == '；'
		isLatinEnder := r == '.' || r == '!' || r == '?'
		if isCJKEnder || isLatinEnder {
			j := i + 1
			for j < len(runes) && closers[runes[j]] {
				j++
			}
			boundary := isCJKEnder
			if !boundary {
				if j >= len(runes) || runes[j] == ' ' || runes[j] == '\t' || runes[j] == '\n' || runes[j] == '\r' {
					boundary = true
				}
			}
			if boundary {
				k := j
				for k < len(runes) && (runes[k] == ' ' || runes[k] == '\t') {
					k++
				}
				emit(k)
				i = k
				continue
			}
		}
		i++
	}
	emit(len(runes))
	return pieces
}

func serializeRun(nodes []*html.Node, elems map[int]*html.Node, counter *int) string {
	var sb strings.Builder
	var write func(n *html.Node)
	write = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			sb.WriteString(n.Data)
		case n.Type == html.ElementNode && protectedTags[n.Data]:
			*counter++
			elems[*counter] = n
			fmt.Fprintf(&sb, "{%d/}", *counter)
		case n.Type == html.ElementNode && inlineTags[n.Data]:
			*counter++
			k := *counter
			elems[k] = n
			fmt.Fprintf(&sb, "{%d}", k)
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				write(c)
			}
			fmt.Fprintf(&sb, "{/%d}", k)
		case n.Type == html.ElementNode:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				write(c)
			}
		}
	}
	for _, n := range nodes {
		write(n)
	}
	return sb.String()
}

func cloneShell(n *html.Node) *html.Node {
	c := &html.Node{
		Type:      html.ElementNode,
		Data:      n.Data,
		DataAtom:  n.DataAtom,
		Namespace: n.Namespace,
	}
	c.Attr = append(c.Attr, n.Attr...)
	return c
}

func deepClone(n *html.Node) *html.Node {
	c := &html.Node{Type: n.Type, Data: n.Data, DataAtom: n.DataAtom, Namespace: n.Namespace}
	c.Attr = append(c.Attr, n.Attr...)
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.AppendChild(deepClone(ch))
	}
	return c
}

func parsePlaceholder(s string, i int) (kind byte, num int, next int, ok bool) {
	j := i + 1
	kind = 'o'
	if j < len(s) && s[j] == '/' {
		kind = 'c'
		j++
	}
	start := j
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == start {
		return 0, 0, 0, false
	}
	num, err := strconv.Atoi(s[start:j])
	if err != nil {
		return 0, 0, 0, false
	}
	if j < len(s) && s[j] == '/' && kind == 'o' {
		kind = 's'
		j++
	}
	if j >= len(s) || s[j] != '}' {
		return 0, 0, 0, false
	}
	return kind, num, j + 1, true
}

func buildNodes(masked string, elems map[int]*html.Node) ([]*html.Node, bool) {
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	stack := []*html.Node{root}
	openNums := []int{}
	flushText := func(txt string) {
		if txt != "" {
			stack[len(stack)-1].AppendChild(&html.Node{Type: html.TextNode, Data: txt})
		}
	}
	i := 0
	textStart := 0
	for i < len(masked) {
		if masked[i] == '{' {
			kind, num, next, ok := parsePlaceholder(masked, i)
			if ok {
				flushText(masked[textStart:i])
				switch kind {
				case 'o':
					el, present := elems[num]
					if !present {
						return nil, false
					}
					clone := cloneShell(el)
					stack[len(stack)-1].AppendChild(clone)
					stack = append(stack, clone)
					openNums = append(openNums, num)
				case 'c':
					if len(openNums) == 0 || openNums[len(openNums)-1] != num {
						return nil, false
					}
					openNums = openNums[:len(openNums)-1]
					stack = stack[:len(stack)-1]
				case 's':
					el, present := elems[num]
					if !present {
						return nil, false
					}
					stack[len(stack)-1].AppendChild(deepClone(el))
				}
				i = next
				textStart = next
				continue
			}
		}
		i++
	}
	flushText(masked[textStart:])
	if len(openNums) != 0 {
		return nil, false
	}
	var out []*html.Node
	for c := root.FirstChild; c != nil; {
		next := c.NextSibling
		root.RemoveChild(c)
		out = append(out, c)
		c = next
	}
	return out, true
}

func isContainer(el *html.Node) bool {
	for c := el.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode, html.CommentNode:
		case html.ElementNode:
			if !isInlineElement(c) {
				return false
			}
		default:
			return false
		}
	}
	return strings.TrimSpace(runText([]*html.Node{el})) != ""
}

func collectUnits(parent *html.Node, units *[]*unit) {
	var run []*html.Node
	flush := func() {
		if len(run) > 0 && strings.TrimSpace(runText(run)) != "" {
			nodes := make([]*html.Node, len(run))
			copy(nodes, run)
			*units = append(*units, &unit{parent: parent, nodes: nodes})
		}
		run = nil
	}
	for c := parent.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode || isInlineElement(c):
			run = append(run, c)
		case c.Type == html.ElementNode && skipTags[c.Data]:
			flush()
		case c.Type == html.ElementNode && hasNoTranslate(c):
			flush()
		case c.Type == html.ElementNode && isContainer(c):
			flush()
			*units = append(*units, &unit{parent: c, nodes: childNodes(c)})
		case c.Type == html.ElementNode:
			flush()
			collectUnits(c, units)
		default:
			flush()
		}
	}
	flush()
}

func childNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if b := findBody(c); b != nil {
			return b
		}
	}
	return nil
}

func (s *Service) TranslateHTML(ctx context.Context, htmlBody, src, tgt string) (string, error) {
	if strings.TrimSpace(htmlBody) == "" {
		return htmlBody, nil
	}
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return "", err
	}
	body := findBody(doc)
	if body == nil {
		return htmlBody, nil
	}
	var units []*unit
	collectUnits(body, &units)
	var segs []Segment
	nextID := 1
	chars := 0
	for _, u := range units {
		u.elems = map[int]*html.Node{}
		counter := 0
		masked := serializeRun(u.nodes, u.elems, &counter)
		u.pieces = splitSentences(masked)
		for _, p := range u.pieces {
			if !p.sentence {
				continue
			}
			if len(segs) >= MaxSegments || chars+len(p.text) > maxChars {
				u.segIDs = append(u.segIDs, 0)
				continue
			}
			chars += len(p.text)
			u.segIDs = append(u.segIDs, nextID)
			segs = append(segs, Segment{ID: nextID, Text: p.text})
			nextID++
		}
	}
	translated := map[int]string{}
	if len(segs) > 0 {
		out, err := s.TranslateSegments(ctx, segs, src, tgt)
		if err != nil {
			return "", err
		}
		for _, sg := range out {
			translated[sg.ID] = sg.Text
		}
	}
	for _, u := range units {
		var sb strings.Builder
		si := 0
		for _, p := range u.pieces {
			if !p.sentence {
				sb.WriteString(p.text)
				continue
			}
			id := u.segIDs[si]
			si++
			if id > 0 {
				if txt, ok := translated[id]; ok {
					sb.WriteString(txt)
					continue
				}
			}
			sb.WriteString(p.text)
		}
		newMasked := sb.String()
		origMasked := ""
		{
			var ob strings.Builder
			for _, p := range u.pieces {
				ob.WriteString(p.text)
			}
			origMasked = ob.String()
		}
		if newMasked == origMasked {
			continue
		}
		newNodes, ok := buildNodes(newMasked, u.elems)
		if !ok {
			continue
		}
		first := u.nodes[0]
		for _, nn := range newNodes {
			u.parent.InsertBefore(nn, first)
		}
		for _, old := range u.nodes {
			u.parent.RemoveChild(old)
		}
	}
	var out strings.Builder
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&out, c); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func (s *Service) TranslateText(ctx context.Context, text, src, tgt string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}
	pieces := splitSentences(text)
	var segs []Segment
	nextID := 1
	chars := 0
	segFor := make([]int, len(pieces))
	for i, p := range pieces {
		if !p.sentence {
			continue
		}
		if len(segs) >= MaxSegments || chars+len(p.text) > maxChars {
			continue
		}
		chars += len(p.text)
		segFor[i] = nextID
		segs = append(segs, Segment{ID: nextID, Text: p.text})
		nextID++
	}
	if len(segs) == 0 {
		return text, nil
	}
	out, err := s.TranslateSegments(ctx, segs, src, tgt)
	if err != nil {
		return "", err
	}
	translated := map[int]string{}
	for _, sg := range out {
		translated[sg.ID] = sg.Text
	}
	var sb strings.Builder
	for i, p := range pieces {
		if segFor[i] > 0 {
			if txt, ok := translated[segFor[i]]; ok {
				sb.WriteString(txt)
				continue
			}
		}
		sb.WriteString(p.text)
	}
	return sb.String(), nil
}
