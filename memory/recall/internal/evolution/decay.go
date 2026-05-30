package evolution

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// NopDecayer is the default decayer.
type NopDecayer struct{}

func (NopDecayer) Apply(context.Context, domain.Scope, time.Time) error { return nil }
