package api

import (
	"testing"
)

func TestRichBlocksWithSnippet(t *testing.T) {
	it := tgItem{ID: 42, Subject: "Hello", From: "A <a@b.c>", Account: "acc@x.y", Snippet: "preview text"}
	blocks := tgRichBlocks(it, "https://t.me/testbot?startapp=42")
	if len(blocks) != 3 {
		t.Fatalf("blocks=%d want 3", len(blocks))
	}
	head, ok := blocks[0].(map[string]any)
	if !ok || head["type"] != "paragraph" {
		t.Fatalf("bad head block: %#v", blocks[0])
	}
	headParts, ok := head["text"].([]any)
	if !ok || len(headParts) != 1 {
		t.Fatalf("head text shape: %#v", head["text"])
	}
	link, ok := headParts[0].(map[string]any)
	if !ok || link["type"] != "url" || link["url"] != "https://t.me/testbot?startapp=42" {
		t.Fatalf("bad title link: %#v", headParts[0])
	}
	bold, ok := link["text"].(map[string]any)
	if !ok || bold["type"] != "bold" || bold["text"] != "Hello" {
		t.Fatalf("bad title text: %#v", link["text"])
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

func TestTitleLink(t *testing.T) {
	if got := tgTitleLink("testbot", 42); got != "https://t.me/testbot?startapp=42" {
		t.Fatalf("title link=%q", got)
	}
	if got := tgTitleLink("", 42); got != "https://namakani.example.com/#/mail/42" {
		t.Fatalf("title link without username=%q", got)
	}
}

func TestRichBlocksEmptySnippet(t *testing.T) {
	it := tgItem{ID: 1, Subject: "S", From: "F", Account: "acc@x.y"}
	blocks := tgRichBlocks(it, "https://t.me/testbot?startapp=1")
	footer := blocks[2].(map[string]any)
	if footer["text"] != "acc@x.y" {
		t.Fatalf("footer text=%#v want plain account", footer["text"])
	}
}
