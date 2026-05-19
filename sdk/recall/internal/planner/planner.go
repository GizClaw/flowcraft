package planner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

type Planner interface {
	Plan(ctx context.Context, input Input) (model.QueryPlan, error)
}

type Input struct {
	Text     string
	Scope    model.Scope
	Entities []string
	Limit    int
}
