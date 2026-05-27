// Package stages owns the three revision pipeline stages
// (lookup_source / attach_revision / revision_save). Together they
// implement Memory.Fork and Memory.Contest.
package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/revision"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// LookupSource is the first revision pipeline stage. It loads the
// canonical source fact (state.SourceFactID) under the scope write-
// lock — which the facade acquires before invoking Run, so the
// stage just calls store.Get; the lock semantics are inherited
// transitively from the caller.
//
// Validation:
//   - SourceFactID must be non-empty.
//   - The source fact must exist in the scope.
//   - For ModeFork the source fact MUST be canonical-active
//     (not superseded and not closed). Forking a retired fact would
//     surface stale state — callers must Save a fresh fact instead.
type LookupSource struct {
	store port.TemporalStore
}

// NewLookupSource constructs the stage.
func NewLookupSource(store port.TemporalStore) *LookupSource {
	return &LookupSource{store: store}
}

// Name implements pipeline.Stage.
func (LookupSource) Name() string { return "revision_lookup_source" }

// Run implements pipeline.Stage.
func (s *LookupSource) Run(ctx context.Context, state *revision.State) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.RevisionDetail{
		Kind:         state.Mode.KindString(),
		Stage:        "revision_lookup_source",
		SourceFactID: state.SourceFactID,
	}
	if state.SourceFactID == "" {
		detail.Latency = time.Since(started)
		return detail, errdefs.Validationf("recall.Revision: source fact id is required")
	}
	src, err := s.store.Get(ctx, state.Scope, state.SourceFactID)
	if err != nil {
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Revision: lookup source: %w", err)
	}
	if state.Mode == revision.ModeFork {
		if !domain.IsProjectable(src, time.Now()) {
			detail.Latency = time.Since(started)
			return detail, errdefs.Validationf("recall.Fork: source fact is not canonical-active")
		}
	}
	state.Source = src
	detail.Latency = time.Since(started)
	return detail, nil
}

var _ pipeline.Stage[*revision.State] = (*LookupSource)(nil)
