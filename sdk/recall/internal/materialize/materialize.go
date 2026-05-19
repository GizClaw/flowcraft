package materialize

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type ContextItem struct {
	Candidate model.Candidate
	Fact      model.TemporalFact
	Evidence  []model.EvidenceRef
}

type Materializer interface {
	Materialize(ctx context.Context, candidates []model.Candidate) ([]ContextItem, error)
}
