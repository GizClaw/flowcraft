package port

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// CandidateAssessor owns the read-side semantic usefulness decision for one
// materialized, policy-visible candidate.
type CandidateAssessor interface {
	Assess(ctx context.Context, in domain.AssessmentInput) (domain.CandidateAssessment, error)
}

// SemanticScorer scores the recall input against materialized candidate
// evidence. Implementations must surface unavailable scorers as errors rather
// than fabricating semantic matches.
type SemanticScorer interface {
	Score(ctx context.Context, input string, candidate domain.ContextItem) (float64, string, error)
}

// SupportReader exposes typed O/A/L links needed by assessment without letting
// the assessment stage scan storage directly.
type SupportReader interface {
	LinksForCandidate(ctx context.Context, scope domain.Scope, item domain.ContextItem) ([]domain.FactLink, error)
}
