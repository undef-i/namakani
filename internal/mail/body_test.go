package mail

import (
	"strings"
	"testing"
)

func TestSnippetStripsQuoteChain(t *testing.T) {
	text := "The deploy finished at noon and all checks passed.\n\nOn Fri, 23 Oct 2026 05:56:04 +0000, Eugene via groups.io wrote:\n> Hi all,\n> I set up a small free hosting service.\n> Feedback welcome.\n"
	want := "The deploy finished at noon and all checks passed."
	if got := MakeSnippet(text, ""); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetStripsSignature(t *testing.T) {
	text := "Please review the attached report before Friday.\n-- \nJane Doe\nChief Everything Officer\nExample Corp\n"
	want := "Please review the attached report before Friday."
	if got := MakeSnippet(text, ""); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetHTMLOnly(t *testing.T) {
	htmlBody := `<html><head><style>.a{color:red}</style><script>var x=1;</script></head><body><p>Hello <b>world</b>, the meeting moved.</p><p>Second paragraph here.</p></body></html>`
	want := "Hello world, the meeting moved. Second paragraph here."
	if got := MakeSnippet("", htmlBody); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetChinese(t *testing.T) {
	text := "王总您好，\n合同模板已按法务意见更新，麻烦今天确认。\n\n在 2026年10月22日，法务部 写道：\n> 请核对第三条。\n"
	want := "合同模板已按法务意见更新，麻烦今天确认。"
	if got := MakeSnippet(text, ""); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetLongWord(t *testing.T) {
	got := MakeSnippet(strings.Repeat("a", 300), "")
	if len([]rune(got)) != snippetMaxRunes {
		t.Fatalf("len=%d want %d (%q)", len([]rune(got)), snippetMaxRunes, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
}

func TestSnippetWordBoundary(t *testing.T) {
	got := MakeSnippet(strings.Repeat("alpha beta gamma delta ", 20), "")
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	if len([]rune(got)) > snippetMaxRunes {
		t.Fatalf("len=%d exceeds %d", len([]rune(got)), snippetMaxRunes)
	}
	trimmed := strings.TrimSuffix(got, "…")
	last := trimmed[strings.LastIndex(trimmed, " ")+1:]
	switch last {
	case "alpha", "beta", "gamma", "delta":
	default:
		t.Fatalf("cut mid-word, last word %q in %q", last, got)
	}
}

func TestSnippetChineseTruncation(t *testing.T) {
	got := MakeSnippet(strings.Repeat("这是一个用于测试截断的中文句子。", 20), "")
	if len([]rune(got)) != snippetMaxRunes {
		t.Fatalf("len=%d want %d", len([]rune(got)), snippetMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
}

func TestSnippetSkipsGreeting(t *testing.T) {
	got := MakeSnippet("Hi John,\nThe 3pm meeting moved to the main room.\n", "")
	want := "The 3pm meeting moved to the main room."
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetKeepsGreetingGluedContent(t *testing.T) {
	got := MakeSnippet("Hi, the meeting moved to the main room at 3pm.\n", "")
	want := "Hi, the meeting moved to the main room at 3pm."
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSnippetGreetingOnlyKept(t *testing.T) {
	if got := MakeSnippet("Hello!\n", ""); got != "Hello!" {
		t.Fatalf("got %q want %q", got, "Hello!")
	}
}

func TestSnippetOutlookHeaderBlock(t *testing.T) {
	text := "Sounds good, shipping it today.\n\nFrom: Xu Fan <xu@example.com>\nSent: Friday, October 23, 2026 9:12 AM\nTo: Team\nSubject: Re: Plan\nOld quoted content follows here.\n"
	want := "Sounds good, shipping it today."
	if got := MakeSnippet(text, ""); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
