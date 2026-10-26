package translate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

type stubEngine struct {
	mu        sync.Mutex
	seen      []string
	calls     int
	batchHook func(segs []Segment) ([]Segment, error)
}

func (e *stubEngine) Key() string { return "stub" }

func (e *stubEngine) Translate(ctx context.Context, segs []Segment, src, tgt string) ([]Segment, error) {
	e.mu.Lock()
	e.calls++
	for _, s := range segs {
		e.seen = append(e.seen, s.Text)
	}
	hook := e.batchHook
	e.mu.Unlock()
	if hook != nil {
		return hook(segs)
	}
	out := make([]Segment, len(segs))
	for i, s := range segs {
		out[i] = Segment{ID: s.ID, Text: "T[" + s.Text + "]"}
	}
	return out, nil
}

func (e *stubEngine) seenJoined() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.seen, "\n")
}

func TestPipelineMasksInlineElementsAndRestores(t *testing.T) {
	eng := &stubEngine{}
	svc := NewService(eng)
	in := `<p>Your <strong>October invoice</strong> is ready.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	seen := eng.seenJoined()
	if !strings.Contains(seen, "{1}") || !strings.Contains(seen, "{/1}") {
		t.Fatalf("engine did not see paired placeholders, saw: %q", seen)
	}
	if strings.Contains(seen, "<strong") || strings.Contains(seen, "<p>") {
		t.Fatalf("engine saw raw tags: %q", seen)
	}
	if !strings.Contains(out, "<strong>") || !strings.Contains(out, "T[") {
		t.Fatalf("restored html lost the inline element or the translation: %q", out)
	}
	if !strings.Contains(out, "October invoice") {
		t.Fatalf("inline text missing after restore: %q", out)
	}
}

func TestPipelineHrefNeverSentToEngine(t *testing.T) {
	eng := &stubEngine{}
	svc := NewService(eng)
	in := `<p>Please <a href="https://example.com/invoice?id=42">review it here</a> today.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	seen := eng.seenJoined()
	if strings.Contains(seen, "example.com") || strings.Contains(seen, "href") {
		t.Fatalf("link target leaked into segments: %q", seen)
	}
	if !strings.Contains(out, `href="https://example.com/invoice?id=42"`) {
		t.Fatalf("href not preserved in output: %q", out)
	}
	if !strings.Contains(out, "review it here") {
		t.Fatalf("anchor text lost: %q", out)
	}
}

func TestPipelineSkipsCodeScriptAndStyle(t *testing.T) {
	eng := &stubEngine{}
	svc := NewService(eng)
	in := `<p>Hello world.</p><pre>code_secret_line</pre><script>var script_secret = 1;</script><style>.style_secret { color: red; }</style><p>Second paragraph here.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	seen := eng.seenJoined()
	for _, secret := range []string{"code_secret_line", "script_secret", "style_secret"} {
		if strings.Contains(seen, secret) {
			t.Fatalf("skipped content %q was sent to the engine: %q", secret, seen)
		}
		if !strings.Contains(out, secret) {
			t.Fatalf("skipped content %q missing from output: %q", secret, out)
		}
	}
	if !strings.Contains(out, "T[Hello world.]") || !strings.Contains(out, "T[Second paragraph here.]") {
		t.Fatalf("visible paragraphs not translated: %q", out)
	}
}

func TestPipelineCorruptPlaceholderFallsBackToOriginalSegment(t *testing.T) {
	eng := &stubEngine{}
	eng.batchHook = func(segs []Segment) ([]Segment, error) {
		out := make([]Segment, len(segs))
		for i, s := range segs {
			txt := "T[" + s.Text + "]"
			if strings.Contains(s.Text, "{1}") {
				txt = strings.ReplaceAll(txt, "{1}", "")
				txt = strings.ReplaceAll(txt, "{/1}", "")
			}
			out[i] = Segment{ID: s.ID, Text: txt}
		}
		return out, nil
	}
	svc := NewService(eng)
	in := `<p>Your <strong>October invoice</strong> is ready.</p><p>Plain closing line.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(out, "Your") || !strings.Contains(out, "<strong>October invoice</strong>") {
		t.Fatalf("corrupted segment did not fall back to its original text: %q", out)
	}
	if strings.Contains(out, "T[Your") {
		t.Fatalf("corrupted segment translation was used anyway: %q", out)
	}
	if !strings.Contains(out, "T[Plain closing line.]") {
		t.Fatalf("healthy segment was not translated: %q", out)
	}
}

func TestPipelineSingleRetryRecoversSegment(t *testing.T) {
	eng := &stubEngine{}
	eng.batchHook = func(segs []Segment) ([]Segment, error) {
		out := make([]Segment, len(segs))
		for i, s := range segs {
			txt := "T[" + s.Text + "]"
			if len(segs) > 1 {
				txt = strings.ReplaceAll(txt, "{1}", "")
				txt = strings.ReplaceAll(txt, "{/1}", "")
			}
			out[i] = Segment{ID: s.ID, Text: txt}
		}
		return out, nil
	}
	svc := NewService(eng)
	in := `<p>Your <strong>October invoice</strong> is ready.</p><p>Plain closing line.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(out, "T[") || !strings.Contains(out, "<strong>October invoice</strong>") {
		t.Fatalf("single-segment retry did not recover the segment: %q", out)
	}
	if eng.calls < 2 {
		t.Fatalf("expected a retry call after the batch, calls = %d", eng.calls)
	}
}

func TestPipelineSegmentCapLeavesRestInOriginal(t *testing.T) {
	eng := &stubEngine{}
	svc := NewService(eng)
	var b strings.Builder
	for i := 0; i < MaxSegments+50; i++ {
		b.WriteString("<p>Sentence number here.</p>")
	}
	out, err := svc.TranslateHTML(context.Background(), b.String(), "en", "zh")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(eng.seen) > MaxSegments {
		t.Fatalf("engine received %d segments, cap is %d", len(eng.seen), MaxSegments)
	}
	if !strings.Contains(out, "T[Sentence number here.]") {
		t.Fatalf("capped output lost translations: %.200q", out)
	}
	if !strings.Contains(out, "<p>Sentence number here.</p>") {
		t.Fatalf("segments beyond the cap were not left in the original: %.200q", out)
	}
}

func TestTranslateTextPreservesParagraphBreaks(t *testing.T) {
	eng := &stubEngine{}
	svc := NewService(eng)
	in := "Hello world.\n\nSecond paragraph here."
	out, err := svc.TranslateText(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("translate text: %v", err)
	}
	if !strings.Contains(out, "T[Hello world.]") || !strings.Contains(out, "T[Second paragraph here.]") {
		t.Fatalf("plain text not translated: %q", out)
	}
	if !strings.Contains(out, "\n\n") {
		t.Fatalf("paragraph break lost: %q", out)
	}
}

func TestTotalEngineFailureSurfacesError(t *testing.T) {
	eng := &stubEngine{}
	eng.batchHook = func(segs []Segment) ([]Segment, error) {
		return nil, errors.New("engine down")
	}
	svc := NewService(eng)
	_, err := svc.TranslateHTML(context.Background(), `<p>Hello world.</p>`, "en", "zh")
	if err == nil {
		t.Fatal("a fully failing engine must surface an error, not a fake translation")
	}
}

func TestSegmentErrorKeepsOriginalAndOthersTranslated(t *testing.T) {
	eng := &stubEngine{}
	eng.batchHook = func(segs []Segment) ([]Segment, error) {
		out := make([]Segment, 0, len(segs))
		for _, s := range segs {
			if strings.Contains(s.Text, "fragile") {
				continue
			}
			out = append(out, Segment{ID: s.ID, Text: "T[" + s.Text + "]"})
		}
		if len(segs) == 1 && strings.Contains(segs[0].Text, "fragile") {
			return nil, errors.New("cannot do this one")
		}
		return out, nil
	}
	svc := NewService(eng)
	in := `<p>A fragile sentence.</p><p>A solid sentence.</p>`
	out, err := svc.TranslateHTML(context.Background(), in, "en", "zh")
	if err != nil {
		t.Fatalf("one bad segment must not fail the whole mail: %v", err)
	}
	if !strings.Contains(out, "A fragile sentence.") {
		t.Fatalf("failed segment did not stay in the original: %q", out)
	}
	if !strings.Contains(out, "T[A solid sentence.]") {
		t.Fatalf("healthy segment not translated: %q", out)
	}
}
