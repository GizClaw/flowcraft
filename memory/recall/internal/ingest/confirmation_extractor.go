package ingest

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func findConfirmationEvidenceSpan(spans []domain.SourceEvidenceSpan, source parameterGrounding, sourceIDs []string, quote string) (domain.SourceEvidenceSpan, bool) {
	ids := cleanSourceIDs(sourceIDs)
	if len(ids) == 0 {
		return domain.SourceEvidenceSpan{}, false
	}
	quote = strings.TrimSpace(quote)
	if quote == "" {
		return domain.SourceEvidenceSpan{}, false
	}
	var best domain.SourceEvidenceSpan
	for _, span := range spans {
		if span.ObservationID == source.ObservationID {
			continue
		}
		if !sourceIDMatches(span, ids) {
			continue
		}
		if source.SessionID != "" && span.SessionID != "" && span.SessionID != source.SessionID {
			continue
		}
		if !source.Timestamp.IsZero() && !span.Timestamp.IsZero() {
			if !span.Timestamp.After(source.Timestamp) {
				continue
			}
		}
		located, ok := locateQuoteInText(span.Text, quote)
		if !ok || strings.TrimSpace(located) == "" {
			continue
		}
		candidate := span
		candidate.Text = located
		if best.SpanID == "" || span.Timestamp.Before(best.Timestamp) {
			best = candidate
		}
		if best.SpanID == "" {
			best = span
		}
	}
	return best, best.SpanID != ""
}
