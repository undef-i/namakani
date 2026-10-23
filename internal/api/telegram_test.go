package api

import (
	"testing"
)

func TestRichBlocksWithSnippet(t *testing.T) {
	it := tgItem{ID: 42, Subject: "Hello", From: "A <a@b.c>", Account: "acc@x.y", Snippet: "preview text"}
	blocks := tgRichBlocks(it)
	if len(blocks) != 3 {
		t.Fatalf("blocks=%d want 3", len(blocks))
	}
	footer, ok := blocks[2].(map[string]any)
	if !ok || footer["type"] != "footer" {
		t.Fatalf("bad footer block: %#v", blocks[2])
	}
	parts, ok := footer["text"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("footer text shape: %#v", footer["text"])
	}
	italic, ok := parts[0].(map[string]any)
	if !ok || italic["type"] != "italic" || italic["text"] != "preview text" {
		t.Fatalf("bad italic part: %#v", parts[0])
	}
	if parts[1] != "\n" || parts[2] != "acc@x.y" {
		t.Fatalf("bad footer tail: %#v", parts)
	}
}

func TestRichBlocksEmptySnippet(t *testing.T) {
	it := tgItem{ID: 1, Subject: "S", From: "F", Account: "acc@x.y"}
	blocks := tgRichBlocks(it)
	footer := blocks[2].(map[string]any)
	if footer["text"] != "acc@x.y" {
		t.Fatalf("footer text=%#v want plain account", footer["text"])
	}
}
