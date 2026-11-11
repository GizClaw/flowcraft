package message

import (
	"encoding/json"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// TestRecordAnchorHelpers pin the one rendering contract the derivation
// source and the answer-side evidence share: the anchor is the record's own
// observed_at or nothing -- the storage time never stands in -- text renders
// as "[anchor] text", and content rendering prefixes the first text part
// while every other part stays where it was.
func TestRecordAnchorHelpers(t *testing.T) {
	const anchor = "2023-05-08T13:56:00Z"
	anchored := Record{Metadata: corememory.Metadata{corememory.ObservedAtMetadataKey: " " + anchor + " "}}
	bare := Record{}
	if got := anchored.ObservedAt(); got != anchor {
		t.Fatalf("ObservedAt = %q, want %q", got, anchor)
	}
	if got := bare.ObservedAt(); got != "" {
		t.Fatalf("bare ObservedAt = %q, want empty", got)
	}
	if got := anchored.AnchoredText("hello"); got != "["+anchor+"] hello" {
		t.Fatalf("AnchoredText = %q", got)
	}
	if got := bare.AnchoredText("hello"); got != "hello" {
		t.Fatalf("bare AnchoredText = %q", got)
	}

	payload := coremessage.DataPart{MediaType: "application/json", Value: json.RawMessage(`{"k":"v"}`)}
	textFirst := coremessage.Content{Parts: []coremessage.Part{
		coremessage.TextPart{Text: "hello"},
		payload,
	}}
	got := anchored.AnchoredContent(textFirst)
	if len(got.Parts) != 2 {
		t.Fatalf("text-first parts = %d, want 2", len(got.Parts))
	}
	if textPart, ok := got.Parts[0].(coremessage.TextPart); !ok || textPart.Text != "["+anchor+"] hello" {
		t.Fatalf("text-first part = %#v", got.Parts[0])
	}
	if _, ok := got.Parts[1].(coremessage.DataPart); !ok {
		t.Fatalf("non-text part changed: %#v", got.Parts[1])
	}
	if textPart, ok := textFirst.Parts[0].(coremessage.TextPart); !ok || textPart.Text != "hello" {
		t.Fatalf("AnchoredContent mutated its input: %#v", textFirst.Parts[0])
	}

	noText := coremessage.Content{Parts: []coremessage.Part{payload}}
	got = anchored.AnchoredContent(noText)
	if len(got.Parts) != 2 {
		t.Fatalf("textless parts = %d, want a leading anchor part plus the original", len(got.Parts))
	}
	if textPart, ok := got.Parts[0].(coremessage.TextPart); !ok || textPart.Text != "["+anchor+"] " {
		t.Fatalf("leading anchor part = %#v", got.Parts[0])
	}
	if got := bare.AnchoredContent(textFirst); got.Parts[0].(coremessage.TextPart).Text != "hello" {
		t.Fatalf("bare AnchoredContent changed the text: %#v", got.Parts[0])
	}
}
