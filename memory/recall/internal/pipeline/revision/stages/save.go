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

// SaveFn is the dependency the Save stage injects in lieu of a
// direct write-pipeline Runner reference. The Memory facade binds it
// to a closure that drives writePreRunner + writePostRunner WITHOUT
// reacquiring the per-scope write lock (the facade holds it across
// the revision Run call), returning the freshly-stored canonical
// fact for state.Created.
//
// Returning the canonical fact (rather than just an id) lets the
// facade hand a populated TemporalFact to its caller without an
// extra store.Get round-trip.
type SaveFn func(ctx context.Context, scope domain.Scope, fact domain.TemporalFact) (domain.TemporalFact, error)

// Save is the third revision pipeline stage. It hands the
// attach_revision-prepared fact to SaveFn and records the resulting
// canonical fact in state.Created.
type Save struct {
	saveFn SaveFn
	store  port.TemporalStore
}

// NewSave constructs the stage. store is kept for a fallback
// store.Get when saveFn returns an empty fact (defensive — the
// memory facade today always populates the returned fact).
func NewSave(saveFn SaveFn, store port.TemporalStore) *Save {
	return &Save{saveFn: saveFn, store: store}
}

// Name implements pipeline.Stage.
func (Save) Name() string { return "revision_save" }

// Run implements pipeline.Stage.
func (s *Save) Run(ctx context.Context, state *revision.State) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.RevisionDetail{
		Kind:         state.Mode.KindString(),
		Stage:        "revision_save",
		SourceFactID: state.SourceFactID,
	}
	if s.saveFn == nil {
		detail.Latency = time.Since(started)
		return detail, errdefs.Internalf("recall.Revision: save fn not wired")
	}
	created, err := s.saveFn(ctx, state.Scope, state.NewFact)
	if err != nil {
		detail.Latency = time.Since(started)
		return detail, fmt.Errorf("recall.Revision: save: %w", err)
	}
	state.Created = created
	detail.CreatedFactID = created.ID
	detail.Latency = time.Since(started)
	return detail, nil
}

var _ pipeline.Stage[*revision.State] = (*Save)(nil)
