package source

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type CandidateSource interface {
	Name() string
	Query(ctx context.Context, plan model.QueryPlan) model.SourceResult
}
