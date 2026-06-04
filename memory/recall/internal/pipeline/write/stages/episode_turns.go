package stages

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallingest "github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// SourceEvidenceSpansForJob revalidates the canonical extractable evidence
// captured at enqueue time against the current observation store. Async semantic
// workers must not reconstruct source turns from rendered episode content or
// snapshot-only state; those shapes are hints and audit material, not extraction
// authority.
func SourceEvidenceSpansForJob(ctx context.Context, observations port.ObservationStore, scope domain.Scope, job port.AsyncSemanticJob) ([]domain.SourceEvidenceSpan, error) {
	if observations == nil {
		return nil, fmt.Errorf("recall async semantic: observation store is required for source evidence validation")
	}
	if len(job.SourceEvidenceSpans) == 0 {
		return nil, fmt.Errorf("recall async semantic: canonical source evidence spans are required")
	}
	out := make([]domain.SourceEvidenceSpan, 0, len(job.SourceEvidenceSpans))
	for i, span := range job.SourceEvidenceSpans {
		if strings.TrimSpace(span.ObservationID) == "" || strings.TrimSpace(span.SpanID) == "" {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] requires canonical observation/span ids", i)
		}
		if strings.TrimSpace(span.Text) == "" {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] text is required", i)
		}
		obs, err := observations.Get(ctx, scope, span.ObservationID)
		if err != nil {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] observation %q: %w", i, span.ObservationID, err)
		}
		if !domain.ScopeVisible(scope, obs.Scope) {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] observation %q is outside scope", i, span.ObservationID)
		}
		if !recallingest.ExtractableEvidenceWindowObservation(obs) {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] observation %q is not extractable raw evidence", i, span.ObservationID)
		}
		validated, ok := sourceEvidenceSpanFromObservation(obs, span)
		if !ok {
			return nil, fmt.Errorf("recall async semantic: source evidence span[%d] %q not found on observation %q", i, span.SpanID, span.ObservationID)
		}
		out = append(out, validated)
	}
	return out, nil
}

func sourceEvidenceSpanFromObservation(obs domain.Observation, requested domain.SourceEvidenceSpan) (domain.SourceEvidenceSpan, bool) {
	spans, err := recallingest.SourceEvidenceSpansFromObservation(obs)
	if err != nil {
		return domain.SourceEvidenceSpan{}, false
	}
	for _, span := range spans {
		if span.SpanID == requested.SpanID &&
			span.ObservationID == requested.ObservationID &&
			span.SourceID == requested.SourceID {
			return span, true
		}
	}
	return domain.SourceEvidenceSpan{}, false
}
