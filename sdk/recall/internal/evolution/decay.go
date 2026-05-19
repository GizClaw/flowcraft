package evolution

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Decayer applies decay / promotion rules to profile slots. Phase 8
// ships a no-op.
type Decayer interface {
	Apply(ctx context.Context, scope model.Scope, now time.Time) error
}

// NopDecayer is the default decayer.
type NopDecayer struct{}

func (NopDecayer) Apply(context.Context, model.Scope, time.Time) error { return nil }
