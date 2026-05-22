package evolution

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

// NopConsolidator is the default consolidator.
type NopConsolidator struct{}

func (NopConsolidator) Consolidate(context.Context, domain.Scope) error { return nil }
