package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/rebuild"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Scan loads the canonical fact snapshot for the rebuild scope.
type Scan struct {
	store port.TemporalStore
}

// NewScan constructs a Scan stage.
func NewScan(store port.TemporalStore) *Scan {
	return &Scan{store: store}
}

// Name implements pipeline.Stage.
func (Scan) Name() string { return "scan" }

// Run implements pipeline.Stage.
func (s *Scan) Run(ctx context.Context, state *rebuild.RebuildState) (diagnostic.StageDetail, error) {
	if state.Scope.RuntimeID == "" {
		return diagnostic.ScanDetail{}, errdefs.Validationf("recall.RebuildAll: scope.runtime_id is required")
	}
	started := time.Now()
	facts, err := s.store.List(ctx, state.Scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		return diagnostic.ScanDetail{
			ScopeKey: state.Scope.RuntimeID,
		}, fmt.Errorf("list canonical facts: %w", err)
	}
	state.Facts = facts
	return diagnostic.ScanDetail{
		ScopeKey:      state.Scope.RuntimeID,
		Total:         len(facts),
		AfterValidity: len(facts),
		Latency:       time.Since(started),
	}, nil
}

var _ pipeline.Stage[*rebuild.RebuildState] = (*Scan)(nil)
