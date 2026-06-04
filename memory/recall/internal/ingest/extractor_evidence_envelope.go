package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// buildExtractorInputEnvelope wraps the extractable evidence JSONL in a small tagged
// protocol. XML-like section tags give the LLM clear boundaries while the
// actual evidence records stay JSONL so ids/text remain machine-shaped.
func buildExtractorInputEnvelope(input port.IngestInput, extractableEvidenceJSONL string) string {
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
	if len(input.ExistingFactHints) > 0 {
		b.WriteString("<existing_fact_hints extractable=\"false\" purpose=\"dedupe_and_disambiguation_only\" format=\"jsonl\">\n")
		for _, f := range input.ExistingFactHints {
			if strings.TrimSpace(f.Content) == "" {
				continue
			}
			lineJSON, _ := json.Marshal(struct {
				Extractable bool   `json:"extractable"`
				Source      string `json:"source"`
				Text        string `json:"text"`
			}{
				Extractable: false,
				Source:      "existing_fact_hint",
				Text:        strings.TrimSpace(f.Content),
			})
			b.Write(lineJSON)
			b.WriteByte('\n')
		}
		b.WriteString("</existing_fact_hints>\n")
	}
	b.WriteString("<extractable_evidence extractable=\"true\" evidence_scope=\"only\" format=\"jsonl\">\n")
	b.WriteString(extractableEvidenceJSONL)
	if !strings.HasSuffix(extractableEvidenceJSONL, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("</extractable_evidence>\n")
	b.WriteString("</extractor_input>")
	return b.String()
}

// buildExtractorUserMessage renders canonical SourceEvidenceSpans into the
// tagged user-message protocol the LLM sees. TurnContext remains available as a
// typed metadata index only; it is not an extractable fallback source.
func buildExtractorUserMessage(input port.IngestInput) (string, map[string]port.TurnContext, bool, error) {
	index, err := buildExtractorTurnIndex(input.Turns)
	if err != nil {
		return "", nil, false, err
	}
	evidenceJSONL := buildExtractorSourceEvidenceJSONL(input.SourceEvidenceSpans)
	if evidenceJSONL == "" {
		return "", index, false, nil
	}
	return buildExtractorInputEnvelope(input, evidenceJSONL), index, true, nil
}

func buildExtractorSourceEvidenceJSONL(spans []port.SourceEvidenceSpan) string {
	if len(spans) == 0 {
		return ""
	}
	var b strings.Builder
	for _, span := range spans {
		if strings.TrimSpace(span.Text) == "" {
			continue
		}
		wire := struct {
			ID            string `json:"id"`
			SegmentID     string `json:"segment_id,omitempty"`
			ObservationID string `json:"observation_id,omitempty"`
			SpanID        string `json:"span_id,omitempty"`
			SourceID      string `json:"source_id,omitempty"`
			SessionID     string `json:"session_id,omitempty"`
			Time          string `json:"time,omitempty"`
			Speaker       string `json:"speaker,omitempty"`
			Role          string `json:"role,omitempty"`
			Text          string `json:"text"`
		}{
			ID:            firstNonEmptyString(span.SourceID, span.SpanID, span.ObservationID),
			SegmentID:     evidenceSegmentID(span),
			ObservationID: span.ObservationID,
			SpanID:        span.SpanID,
			SourceID:      span.SourceID,
			SessionID:     span.SessionID,
			Speaker:       strings.TrimSpace(span.Speaker),
			Role:          strings.TrimSpace(span.Role),
			Text:          strings.TrimSpace(span.Text),
		}
		if !span.Timestamp.IsZero() {
			wire.Time = span.Timestamp.UTC().Format(time.RFC3339)
		}
		line, err := json.Marshal(wire)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func buildExtractorTurnIndex(turns []port.TurnContext) (map[string]port.TurnContext, error) {
	if len(turns) == 0 {
		return nil, nil
	}
	index := make(map[string]port.TurnContext, len(turns))
	for i, t := range turns {
		id := turnLLMID(t)
		if id == "" {
			id = fmt.Sprintf("turn-%d", i+1)
		}
		if _, dup := index[id]; dup {
			return nil, fmt.Errorf("duplicate source turn id %q", id)
		}
		index[id] = t
	}
	return index, nil
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

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func evidenceSegmentID(span port.SourceEvidenceSpan) string {
	return firstNonEmptyString(span.SpanID, span.SourceID, span.ObservationID)
}
