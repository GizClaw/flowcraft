package memory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func documentWithText(text string) memory.Document {
	return memory.Document{
		Scope:          memory.Scope{RuntimeID: "runtime"},
		DatasetID:      "dataset",
		DocumentID:     "document",
		IdempotencyKey: "ingest",
		Content:        message.Content{Parts: []message.Part{message.TextPart{Text: text}}},
		Provenance:     []memory.SourceRef{{Kind: memory.SourceExternal, ID: "uri:document"}},
	}
}

// TestDocumentRejectsTextAboveTheCeiling pins the contract's gate: a document
// is derived from its runes, so the number of them is what makes a document a
// bounded amount of work. The refusal is a request error -- the payload is
// wrong, not the service -- and it names both numbers, because a host that has
// to split a document is told by how much.
func TestDocumentRejectsTextAboveTheCeiling(t *testing.T) {
	if err := documentWithText(strings.Repeat("a", memory.MaxDocumentRunes)).Validate(); err != nil {
		t.Fatalf("a document at the ceiling was rejected: %v", err)
	}
	// The ceiling counts runes, not bytes: the same number of three-byte
	// characters is three times the payload and still one document.
	if err := documentWithText(strings.Repeat("字", memory.MaxDocumentRunes)).Validate(); err != nil {
		t.Fatalf("a document of multi-byte runes at the ceiling was rejected: %v", err)
	}
	err := documentWithText(strings.Repeat("a", memory.MaxDocumentRunes+1)).Validate()
	if !memory.IsKind(err, memory.KindInvalidRequest) || !errdefs.IsValidation(err) {
		t.Fatalf("oversized document error = %v, want an invalid request", err)
	}
	// The contract's message stays capability-shaped and the detail rides in
	// the cause, like every other validation error here.
	detail := errorChain(err)
	for _, want := range []string{"1000001", "1000000", "ceiling"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("error chain does not name %q: %s", want, detail)
		}
	}
}

// errorChain renders every message the error unwraps to, which is where the
// contract keeps the detail a neutral capability error does not carry.
func errorChain(err error) string {
	var messages []string
	for err != nil {
		messages = append(messages, err.Error())
		err = errors.Unwrap(err)
	}
	return strings.Join(messages, " | ")
}

// TestDocumentTextRunesCountsTheTextAndOnlyTheText pins what the ceiling
// measures. It is the number the deriver reads, summed over the parts in either
// of the two forms a caller may hand them in, and media rides along uncounted:
// it is stored, not derived.
func TestDocumentTextRunesCountsTheTextAndOnlyTheText(t *testing.T) {
	image, err := media.NewImageBytes([]byte(strings.Repeat("x", 512)), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	content := message.Content{Parts: []message.Part{
		message.TextPart{Text: "abc"},
		&message.TextPart{Text: "defg"},
		message.ImagePart{Source: image},
		(*message.TextPart)(nil),
	}}
	if got := memory.DocumentTextRunes(content); got != 7 {
		t.Fatalf("DocumentTextRunes = %d, want 7 (the text parts, in either form)", got)
	}
	if got := memory.DocumentTextRunes(message.Content{}); got != 0 {
		t.Fatalf("empty content counted %d runes", got)
	}
}
