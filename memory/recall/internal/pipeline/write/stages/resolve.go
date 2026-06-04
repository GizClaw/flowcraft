package stages

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// Resolve applies the ConflictResolver against a read-only store
// view. It mirrors the legacy runSave block that wrapped resolver
// invocation under the per-scope write lock.
//
// The stage implements Conditional so that a nil resolver (the
// caller explicitly opted out via Options) is reported as a single
// Skipped diagnostic.
//
// An empty resolution (no facts to append and no closes) short-
// circuits the pipeline so downstream stages do not run with an
// empty work set, matching legacy `if len(resolution.Facts) == 0 {
// return SaveResult{}, trace, nil }`.
type Resolve struct {
	resolver port.ConflictResolver
	store    port.TemporalStore
}

// NewResolve constructs a Resolve stage. A nil resolver makes the
// stage Skip on every call.
func NewResolve(resolver port.ConflictResolver, store port.TemporalStore) *Resolve {
	return &Resolve{resolver: resolver, store: store}
}

// Name implements pipeline.Stage.
func (Resolve) Name() string { return "resolve" }

// Skip implements pipeline.Conditional.
func (s *Resolve) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if s.resolver == nil {
		state.Resolution = domain.Resolution{Facts: state.Ingest.Facts}
		return true, diagnostic.ResolveDetail{
			Candidates: len(state.Ingest.Facts),
			Appended:   len(state.Ingest.Facts),
			FactStats:  computeFactStats(state.Ingest.Facts),
		}
	}
	if asyncStructuredLegInactive(state) {
		return true, diagnostic.ResolveDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (s *Resolve) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	view := ingest.StoreView{
		FindByMergeKeyFn: s.store.FindByMergeKey,
		GetFn:            s.store.Get,
	}
	res, err := s.resolver.ResolveConflicts(ctx, view, state.Ingest.Facts)
	if err != nil {
		state.FailedStage = "resolve"
		return diagnostic.ResolveDetail{Candidates: len(state.Ingest.Facts)}, fmt.Errorf("recall.Save: resolve conflicts: %w", err)
	}
	state.Resolution = res
	detail := diagnostic.ResolveDetail{
		Candidates: len(state.Ingest.Facts),
		Appended:   len(res.Facts),
		Closed:     len(res.Closes),
		Superseded: len(res.Closes),
		FactStats:  computeFactStats(res.Facts),
	}
	for _, f := range state.Ingest.Facts {
		if rev, ok := domain.RevisionOf(f); ok {
			switch rev.Kind {
			case domain.RevisionFork:
				detail.Forked++
			case domain.RevisionContest:
				detail.Contested++
			}
		}
		if len(f.Supersedes) > 0 {
			detail.Merged++
		}
	}
	if len(res.Facts) == 0 && len(res.Closes) == 0 {
		return detail, pipeline.ShortCircuitWith("empty_resolution")
	}
	return detail, nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*Resolve)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*Resolve)(nil)
)
