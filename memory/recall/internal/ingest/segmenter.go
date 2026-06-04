package ingest

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"strings"
)

type evidenceSegment struct {
	Span domain.SourceEvidenceSpan
}

func segmentObservationSpans(spans []domain.SourceEvidenceSpan) []evidenceSegment {
	out := make([]evidenceSegment, 0, len(spans))
	for _, span := range spans {
		if strings.TrimSpace(span.Text) == "" {
			continue
		}
		out = append(out, evidenceSegment{Span: span})
	}
	return out
}
