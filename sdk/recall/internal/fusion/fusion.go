package fusion

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type Fuser interface {
	Fuse(ctx context.Context, results []model.SourceResult, opts Options) ([]model.Candidate, error)
}

type Options struct {
	Weights map[string]float64
	Limit   int
}
