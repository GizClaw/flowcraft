package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// RankInput is the assessment-passed candidate pool the ranker reorders.
// AssessmentScores is aligned with Items; Candidate.Score remains source-local
// discovery provenance and must not be read as rank input.
type RankInput struct {
	Items            []domain.ContextItem
	AssessmentScores []float64
	Intent           domain.QueryIntent
	FinalCap         int
	Now              time.Time
}

// RankOutput is the ranked pool plus counters for RankDetail telemetry.
type RankOutput struct {
	Items                  []domain.ContextItem
	RankScores             []float64
	BoostsApplied          int
	TimeDecayApplied       int
	SupersededDecayApplied int
}

// Ranker applies deterministic confidence/feedback adjustments, optional time
// decay, and supersede penalties after centralized candidate assessment.
type Ranker interface {
	Rank(ctx context.Context, in RankInput) RankOutput
}

// Reranker is the optional context_pack step that reorders a
// candidate Hit slice by a stronger relevance signal than the
// deterministic Ranker alone (typically an LLM call or
// cross-encoder).
//
// It runs after materialize / Ranker adjustments and before the final
// TotalCap is applied so the reranker sees the widest fused pool
// (typically 2× the requested topK). Implementations:
//
//   - SHOULD return the same Hit values reordered (no add / drop).
//     Hits beyond an implementation's batch capacity may be
//     appended verbatim at the tail.
//   - MAY surface a non-nil error to signal a degraded reorder; the
//     caller is expected to fall back to the input ordering.
//
// Reranking is intentionally NOT in the default pipeline: it costs
// a per-Recall round-trip to a model the SDK does not own. The
// facade opts in via recall.WithReranker.
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []domain.Hit) ([]domain.Hit, error)
}

// IntentReranker is an optional structured-query extension. Context packing
// prefers it when available so Subject/Predicate/Object-only recalls are not
// flattened to an empty query string.
type IntentReranker interface {
	RerankWithIntent(ctx context.Context, intent domain.QueryIntent, hits []domain.Hit) ([]domain.Hit, error)
}
