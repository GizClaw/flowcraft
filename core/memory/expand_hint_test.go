package memory

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/message"
)

func TestContextItemExpandHintCloneAndValidation(t *testing.T) {
	item := ContextItem{
		ID:          "summary",
		Kind:        ContextSummary,
		Score:       1,
		Content:     message.Content{Parts: []message.Part{message.TextPart{Text: "summary"}}},
		Sources:     []SourceRef{{Kind: SourceMessage, ID: "conversation/message", Revision: "1"}},
		Metadata:    Metadata{"key": "value"},
		SourceClass: ContextSourceSummary,
		Hint: &ExpandHint{
			Topics:         []string{"architecture"},
			SourceRefs:     []SourceRef{{Kind: SourceMessage, ID: "conversation/message", Revision: "1"}},
			Range:          ContextRange{StartSequence: 1, EndSequence: 2},
			PreferredLevel: 1,
		},
	}
	if err := item.Validate(); err != nil {
		t.Fatal(err)
	}
	cloned := item.Clone()
	cloned.Content.Parts[0] = message.TextPart{Text: "mutated"}
	cloned.Sources[0].ID = "mutated"
	cloned.Metadata["key"] = "mutated"
	cloned.Hint.Topics[0] = "mutated"
	cloned.Hint.SourceRefs[0].ID = "mutated"
	if got := item.Content.Parts[0].(message.TextPart).Text; got != "summary" {
		t.Fatalf("content = %q, want summary", got)
	}
	if item.Sources[0].ID != "conversation/message" || item.Metadata["key"] != "value" {
		t.Fatal("context item clone aliases source or metadata")
	}
	if item.Hint.Topics[0] != "architecture" || item.Hint.SourceRefs[0].ID != "conversation/message" {
		t.Fatal("context item clone aliases expand hint")
	}

	bad := item
	bad.Hint = &ExpandHint{Range: ContextRange{StartSequence: 2, EndSequence: 1}, PreferredLevel: 4}
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid hint accepted")
	}
	bad.Hint = &ExpandHint{
		SourceRefs: []SourceRef{{Kind: SourceMessage, ID: "message"}},
		Range:      ContextRange{StartTime: time.Now(), EndTime: time.Now().Add(-time.Second)},
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid time range accepted")
	}
}
