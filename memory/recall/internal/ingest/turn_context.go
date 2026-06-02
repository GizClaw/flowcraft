package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// buildExtractorInputEnvelope wraps the source JSONL in a small tagged
// protocol. XML-like section tags give the LLM clear boundaries while the
// actual turn records stay JSONL so ids/text remain machine-shaped.
func buildExtractorInputEnvelope(input port.IngestInput, sourceTurnsJSONL string) string {
	var b strings.Builder
	b.WriteString("<extractor_input>\n")
	if len(input.RecentMessages) > 0 {
		b.WriteString("<recent_context extractable=\"false\" purpose=\"disambiguation_only\" format=\"jsonl\">\n")
		for _, m := range input.RecentMessages {
			if !m.Time.IsZero() {
				lineJSON, _ := json.Marshal(struct {
					Extractable bool   `json:"extractable"`
					Source      string `json:"source"`
					Role        string `json:"role,omitempty"`
					Speaker     string `json:"speaker,omitempty"`
					Time        string `json:"time,omitempty"`
					Text        string `json:"text"`
				}{
					Extractable: false,
					Source:      "recent_context",
					Role:        strings.TrimSpace(m.Role),
					Speaker:     strings.TrimSpace(m.Speaker),
					Time:        m.Time.UTC().Format(time.RFC3339),
					Text:        strings.TrimSpace(m.Text),
				})
				b.Write(lineJSON)
			} else {
				lineJSON, _ := json.Marshal(struct {
					Extractable bool   `json:"extractable"`
					Source      string `json:"source"`
					Role        string `json:"role,omitempty"`
					Speaker     string `json:"speaker,omitempty"`
					Text        string `json:"text"`
				}{
					Extractable: false,
					Source:      "recent_context",
					Role:        strings.TrimSpace(m.Role),
					Speaker:     strings.TrimSpace(m.Speaker),
					Text:        strings.TrimSpace(m.Text),
				})
				b.Write(lineJSON)
			}
			b.WriteByte('\n')
		}
		b.WriteString("</recent_context>\n")
	}
	if len(input.ExistingFactsAnchor) > 0 {
		b.WriteString("<existing_memory_anchors extractable=\"false\" purpose=\"dedupe_and_disambiguation_only\" format=\"jsonl\">\n")
		for _, f := range input.ExistingFactsAnchor {
			if strings.TrimSpace(f.Content) == "" {
				continue
			}
			lineJSON, _ := json.Marshal(struct {
				Extractable bool   `json:"extractable"`
				Source      string `json:"source"`
				Text        string `json:"text"`
			}{
				Extractable: false,
				Source:      "existing_memory_anchor",
				Text:        strings.TrimSpace(f.Content),
			})
			b.Write(lineJSON)
			b.WriteByte('\n')
		}
		b.WriteString("</existing_memory_anchors>\n")
	}
	b.WriteString("<source_turns extractable=\"true\" evidence_scope=\"only\" format=\"jsonl\">\n")
	b.WriteString(sourceTurnsJSONL)
	if !strings.HasSuffix(sourceTurnsJSONL, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("</source_turns>\n")
	b.WriteString("</extractor_input>")
	return b.String()
}

// buildExtractorUserMessage renders Input.Turns into the tagged user-message
// protocol the LLM sees. turnIndex is a fast id-lookup the response parser
// uses to enrich evidence refs with typed timestamp / role signals.
func buildExtractorUserMessage(input port.IngestInput) (string, map[string]port.TurnContext, bool, error) {
	sourceTurnsJSONL, index, ok, err := buildExtractorSourceTurnsJSONL(input)
	if !ok || err != nil {
		return "", nil, false, err
	}
	return buildExtractorInputEnvelope(input, sourceTurnsJSONL), index, true, nil
}

// buildExtractorSourceTurnsJSONL renders only the extractable source turns.
// Turns with empty ID get a synthetic "turn-N" so prose-only callers (a
// single TurnContext with just Text) still produce a valid shape the model can
// cite. ok=false means "no usable input — skip the LLM call".
func buildExtractorSourceTurnsJSONL(input port.IngestInput) (string, map[string]port.TurnContext, bool, error) {
	if len(input.Turns) == 0 {
		return "", nil, false, nil
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
		if _, dup := index[id]; dup {
			return "", nil, false, fmt.Errorf("duplicate source turn id %q", id)
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
		return "", nil, false, nil
	}
	return b.String(), index, true, nil
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
