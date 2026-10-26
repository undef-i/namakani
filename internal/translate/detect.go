package translate

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

func StripTagsForDetect(htmlBody string) string {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return sb.String()
}

func Detect(text string) string {
	var han, kana, hangul, cyrillic, arabic, latin int
	n := 0
	for _, r := range text {
		if n >= 4000 {
			break
		}
		n++
		switch {
		case unicode.Is(unicode.Han, r):
			han++
		case unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			kana++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Arabic, r):
			arabic++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	switch {
	case hangul > 0:
		return "ko"
	case kana > 0:
		return "ja"
	case han > 0 && han*2 >= latin:
		return "zh"
	case cyrillic > latin && cyrillic > han:
		return "ru"
	case arabic > latin && arabic > han:
		return "ar"
	case latin > 0:
		return "en"
	case han > 0:
		return "zh"
	}
	return ""
}
