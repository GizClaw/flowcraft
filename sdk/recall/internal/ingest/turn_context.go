package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// formatExtractorContextPrefix renders RecentMessages /
// ExistingFactsAnchor as a disambiguation preamble the LLM sees ahead
// of the main JSONL turn block.
func formatExtractorContextPrefix(input port.IngestInput) string {
	var b strings.Builder
	if len(input.RecentMessages) > 0 {
		b.WriteString("Recent conversation context (for disambiguation only, do not extract duplicate memories from this block):\n")
		for i, m := range input.RecentMessages {
			line := struct {
				Role    string `json:"role,omitempty"`
				Speaker string `json:"speaker,omitempty"`
				Text    string `json:"text"`
			}{
				Role:    strings.TrimSpace(m.Role),
				Speaker: strings.TrimSpace(m.Speaker),
				Text:    strings.TrimSpace(m.Text),
			}
			if !m.Time.IsZero() {
				lineJSON, _ := json.Marshal(struct {
					Role    string `json:"role,omitempty"`
					Speaker string `json:"speaker,omitempty"`
					Time    string `json:"time,omitempty"`
					Text    string `json:"text"`
				}{line.Role, line.Speaker, m.Time.UTC().Format(time.RFC3339), line.Text})
				b.Write(lineJSON)
			} else {
				lineJSON, _ := json.Marshal(line)
				b.Write(lineJSON)
			}
			b.WriteByte('\n')
			_ = i
		}
		b.WriteString("\n")
	}
	if len(input.ExistingFactsAnchor) > 0 {
		b.WriteString("Existing memory anchors (do not re-extract identical facts):\n")
		for _, f := range input.ExistingFactsAnchor {
			if strings.TrimSpace(f.Content) == "" {
				continue
			}
			b.WriteString("- ")
			b.WriteString(strings.TrimSpace(f.Content))
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildExtractorUserMessage renders Input.Turns into the canonical
// JSONL wire shape the LLM sees. turnIndex is a fast id-lookup the
// response parser uses to enrich evidence refs with the typed
// timestamp / role signals the LLM does not need to repeat. Turns
// with empty ID get a synthetic "turn-N" so prose-only callers (a
// single TurnContext with just Text) still produce a valid wire
// shape the model can cite. ok=false means "no usable input — skip
// the LLM call" so the extractor degrades cleanly when callers only
// supply structured Facts.
func buildExtractorUserMessage(input port.IngestInput) (string, map[string]port.TurnContext, bool) {
	if len(input.Turns) == 0 {
		return "", nil, false
	}
	index := make(map[string]port.TurnContext, len(input.Turns))
	var b strings.Builder
	written := 0
	for i, t := range input.Turns {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		id := turnLLMID(t)
		if id == "" {
			id = fmt.Sprintf("turn-%d", i+1)
		}
		wire := struct {
			ID      string `json:"id"`
			Time    string `json:"time,omitempty"`
			Speaker string `json:"speaker,omitempty"`
			Role    string `json:"role,omitempty"`
			Text    string `json:"text"`
		}{
			ID:      id,
			Speaker: strings.TrimSpace(t.Speaker),
			Role:    strings.TrimSpace(t.Role),
			Text:    strings.TrimSpace(t.Text),
		}
		if !t.Time.IsZero() {
			wire.Time = t.Time.UTC().Format(time.RFC3339)
		}
		line, err := json.Marshal(wire)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
		written++
		index[id] = t
	}
	if written == 0 {
		return "", nil, false
	}
	return b.String(), index, true
}

// turnLLMID is the id the LLM sees and cites. We prefer the
// adapter's EvidenceID (the adapter-meaningful handle that
// downstream consumers like evaluation harnesses expect) over the
// internal ID so evidence scoring can match without an extra alias
// map.
func turnLLMID(t port.TurnContext) string {
	if id := strings.TrimSpace(t.EvidenceID); id != "" {
		return id
	}
	return strings.TrimSpace(t.ID)
}
