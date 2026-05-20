package read

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// PublicRecallTrace materialises the domain.RecallTrace the evolution
// runner and explain path consume from the in-flight ReadState.
func PublicRecallTrace(state *ReadState) domain.RecallTrace {
	if state == nil {
		return domain.RecallTrace{}
	}
	out := domain.RecallTrace{
		TotalLatency: time.Since(state.StartedAt),
	}
	if state.Plan != nil {
		out.Plan = *state.Plan
	}
	if state.Trace != nil {
		t := state.Trace
		out.Plan = t.Plan
		out.Sources = append([]domain.SourceTrace(nil), t.Sources...)
		out.FusedCandidates = t.FusedCandidates
		out.Drops = append([]diagnostic.CandidateDrop(nil), t.Drops...)
		out.Materialized = t.Materialized
		out.Reranked = t.Reranked
		out.RerankErr = t.RerankErr
		out.Stages = append([]diagnostic.StageDiagnostic(nil), t.Stages...)
	}
	if state.RerankErr != nil && out.RerankErr == "" {
		out.RerankErr = state.RerankErr.Error()
	}
	if state.Reranked > 0 && out.Reranked == 0 {
		out.Reranked = state.Reranked
	}
	return out
}
