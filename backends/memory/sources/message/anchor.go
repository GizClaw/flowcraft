package message

import (
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// ObservedAt returns the record's observation anchor -- the observed_at
// metadata a writer stamped when the message was observed -- or "" when the
// record carries none. The record's CreatedAt is its storage time and is
// deliberately not a fallback: over a bulk import it would degrade to an
// "import day" label, which is worse than no anchor at all.
func (record Record) ObservedAt() string {
	return strings.TrimSpace(record.Metadata[corememory.ObservedAtMetadataKey])
}

// AnchoredText returns text prefixed with the record's anchor in the shape the
// derivation source renders ("[<anchor>] text"), or text unchanged when the
// record carries no anchor.
func (record Record) AnchoredText(text string) string {
	anchor := record.ObservedAt()
	if anchor == "" {
		return text
	}
	return "[" + anchor + "] " + text
}

// AnchoredContent returns content with the same prefix folded into its first
// text part, preserving the order and reusing the other parts; a content
// without a text part gains a leading text part that carries the anchor. A
// record without an anchor gets content back unchanged.
func (record Record) AnchoredContent(content coremessage.Content) coremessage.Content {
	anchor := record.ObservedAt()
	if anchor == "" {
		return content
	}
	prefix := "[" + anchor + "] "
	parts := make([]coremessage.Part, 0, len(content.Parts)+1)
	prefixed := false
	for _, part := range content.Parts {
		if textPart, ok := part.(coremessage.TextPart); ok && !prefixed {
			prefixed = true
			parts = append(parts, coremessage.TextPart{Text: prefix + textPart.Text})
			continue
		}
		parts = append(parts, part)
	}
	if !prefixed {
		parts = append([]coremessage.Part{coremessage.TextPart{Text: prefix}}, parts...)
	}
	return coremessage.Content{Parts: parts}
}
