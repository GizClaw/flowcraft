package compiler

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Compiler owns write-time memory compilation. Concrete extractors,
// normalizers, conflict detectors, and policy hooks will compose behind this
// boundary.
type Compiler interface {
	Compile(ctx context.Context, input Input) ([]model.TemporalFact, error)
}

type Input struct {
	Scope model.Scope
	Text  string
}
