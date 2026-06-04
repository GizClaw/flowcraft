package ingest

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ResolveEvidenceWindowRefs validates caller-declared raw observation pointers
// and projects them into source spans that extraction can cite.
func ResolveEvidenceWindowRefs(ctx context.Context, observations port.ObservationStore, scope domain.Scope, refs []domain.EvidenceWindowRef) ([]domain.SourceEvidenceSpan, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if observations == nil {
		return nil, errdefs.Validationf("recall.Save: evidence_window_refs require observation store")
	}
	out := make([]domain.SourceEvidenceSpan, 0, len(refs))
	for i, ref := range refs {
		obsID := strings.TrimSpace(ref.ObservationID)
		if obsID == "" {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d].observation_id is required", i)
		}
		obs, err := observations.Get(ctx, scope, obsID)
		if err != nil {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] observation %q: %v", i, obsID, err)
		}
		if !domain.ScopeVisible(scope, obs.Scope) {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] observation %q is outside scope", i, obsID)
		}
		if !ExtractableEvidenceWindowObservation(obs) {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] observation %q is not extractable raw evidence", i, obsID)
		}
		if ref.SourceID != "" && strings.TrimSpace(ref.SourceID) != obs.SourceID {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] source_id mismatch", i)
		}
		if ref.SessionID != "" && strings.TrimSpace(ref.SessionID) != obs.SessionID {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] session_id mismatch", i)
		}
		spans, err := SourceEvidenceSpansFromObservation(obs)
		if err != nil {
			return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] observation %q spans: %v", i, obsID, err)
		}
		if spanID := strings.TrimSpace(ref.SpanID); spanID != "" {
			spans = filterSourceEvidenceSpan(spans, spanID)
			if len(spans) == 0 {
				return nil, errdefs.Validationf("recall.Save: evidence_window_refs[%d] span %q not found on observation %q", i, spanID, obsID)
			}
		}
		out = append(out, spans...)
	}
	return out, nil
}

func ExtractableEvidenceWindowObservation(obs domain.Observation) bool {
	switch obs.Kind {
	case domain.ObservationKindTurn, domain.ObservationKindDocument:
	default:
		return false
	}
	if metadataBool(obs.Metadata, "forgotten") ||
		metadataBool(obs.Metadata, "tombstone") ||
		metadataBool(obs.Metadata, "deleted") ||
		metadataBool(obs.Metadata, "closed") ||
		metadataBool(obs.Metadata, "expired") ||
		metadataBool(obs.Metadata, "hard_deleted") {
		return false
	}
	return true
}

func metadataBool(meta map[string]any, key string) bool {
	if len(meta) == 0 {
		return false
	}
	v, ok := meta[key]
	if !ok {
		return false
	}
	switch typed := v.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func SourceEvidenceSpansFromObservations(observations []domain.Observation) ([]domain.SourceEvidenceSpan, error) {
	var out []domain.SourceEvidenceSpan
	for i, obs := range observations {
		spans, err := SourceEvidenceSpansFromObservation(obs)
		if err != nil {
			return nil, errdefs.Validationf("observation[%d] %q spans: %v", i, obs.ID, err)
		}
		out = append(out, spans...)
	}
	return out, nil
}

func SourceEvidenceSpansFromObservation(obs domain.Observation) ([]domain.SourceEvidenceSpan, error) {
	ts := obs.ObservedAt
	if ts.IsZero() {
		ts = obs.ReceivedAt
	}
	out := make([]domain.SourceEvidenceSpan, 0, len(obs.Spans))
	for i, span := range obs.Spans {
		if strings.TrimSpace(span.Text) == "" {
			continue
		}
		if err := validateObservationSpanOffsets(obs, span); err != nil {
			return nil, errdefs.Validationf("span[%d] %q: %v", i, span.ID, err)
		}
		out = append(out, domain.SourceEvidenceSpan{
			ObservationID: obs.ID,
			SpanID:        span.ID,
			SourceID:      obs.SourceID,
			SessionID:     obs.SessionID,
			Scope:         obs.Scope,
			Kind:          span.Kind,
			Text:          span.Text,
			Start:         span.Start,
			End:           span.End,
			Role:          obs.Role,
			Speaker:       obs.Speaker,
			Timestamp:     ts,
		})
	}
	return out, nil
}

func validateObservationSpanOffsets(obs domain.Observation, span domain.ObservationSpan) error {
	if obs.ID == "" {
		return errdefs.Validationf("observation id is required")
	}
	if span.ObservationID == "" {
		return errdefs.Validationf("span observation_id is required")
	}
	if span.ObservationID != obs.ID {
		return errdefs.Validationf("span observation_id %q does not match observation %q", span.ObservationID, obs.ID)
	}
	if strings.TrimSpace(span.SourceID) != "" && strings.TrimSpace(obs.SourceID) != "" && strings.TrimSpace(span.SourceID) != strings.TrimSpace(obs.SourceID) {
		return errdefs.Validationf("span source_id %q does not match observation source_id %q", span.SourceID, obs.SourceID)
	}
	if span.Start < 0 || span.End < 0 {
		return errdefs.Validationf("offsets must be non-negative")
	}
	if span.End <= span.Start {
		return errdefs.Validationf("end must be greater than start")
	}
	if strings.TrimSpace(obs.Text) == "" {
		return nil
	}
	if span.End > len(obs.Text) {
		return errdefs.Validationf("end offset %d exceeds observation text length %d", span.End, len(obs.Text))
	}
	if obs.Text[span.Start:span.End] != span.Text {
		return errdefs.Validationf("span text does not match observation text offsets")
	}
	return nil
}

func filterSourceEvidenceSpan(spans []domain.SourceEvidenceSpan, spanID string) []domain.SourceEvidenceSpan {
	out := spans[:0]
	for _, span := range spans {
		if span.SpanID == spanID {
			out = append(out, span)
		}
	}
	return out
}
