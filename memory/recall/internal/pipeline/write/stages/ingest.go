package stages

import (
	"context"
	"slices"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// EntitySnapshotFunc is the closure shape the ingest stage uses to
// lift canonical entity hints for the structurizer. It is supplied
// at construction (the facade owns iterating its projection list).
// Nil is permitted — the structurizer simply degrades to NER-only
// extraction, the same path the very first Save in a scope already
// takes.
type EntitySnapshotFunc func(scope domain.Scope) []port.EntitySnapshot

// Ingest is the structurize + governance-filter stage. It mirrors
// the legacy runSave block that called snapshotKnownEntities then
// port.Ingestor.Compile, populating state.KnownEntities and
// state.Ingest so resolve can run downstream.
//
// Empty extractor output is a normal terminal outcome (no facts to
// save); the stage returns pipeline.ShortCircuit so later stages do
// not run and the pipeline returns nil — matching the legacy
// `if len(compiled.Facts) == 0 { return SaveResult{}, trace, nil }`
// early exit.
type Ingest struct {
	ingestor port.Ingestor
	snapshot EntitySnapshotFunc
}

// NewIngest constructs an Ingest stage from the wired ingestor and
// an optional entity snapshot lookup.
func NewIngest(ingestor port.Ingestor, snapshot EntitySnapshotFunc) *Ingest {
	return &Ingest{ingestor: ingestor, snapshot: snapshot}
}

// Name implements pipeline.Stage.
func (Ingest) Name() string { return "ingest" }

// Run implements pipeline.Stage.
func (s *Ingest) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	if s.snapshot != nil {
		state.KnownEntities = s.snapshot(state.Scope)
	}
	started := time.Now()
	res, err := s.ingestor.Compile(ctx, port.IngestInput{
		Scope:               state.Scope,
		Facts:               state.Facts,
		Turns:               state.Turns,
		ObservedAt:          state.ObservedAt,
		KnownEntities:       state.KnownEntities,
		Now:                 state.Now,
		Tier:                state.Tier,
		RecentMessages:      state.RecentMessages,
		ExistingFactsAnchor: state.ExistingFactsAnchor,
	})
	latency := time.Since(started)
	if err != nil {
		state.FailedStage = "ingest"
		return diagnostic.IngestDetail{
			InputTurns:          len(state.Turns),
			ExtractedFacts:      len(res.Facts),
			ExtractorLatency:    latency,
			ExtractorTokenUsage: res.ExtractorTokenUsage,
			ExtractorGuard:      res.ExtractorGuard,
		}, err
	}
	state.Ingest = res
	detail := diagnostic.IngestDetail{
		InputTurns:             len(state.Turns),
		ExtractedFacts:         len(res.Facts),
		DroppedByPolicy:        countDroppedReason(res.Dropped, "policy:reject", "governance:reject"),
		DroppedByValidation:    countDroppedReason(res.Dropped, "validation:reject"),
		DroppedByDedup:         countDroppedReason(res.Dropped, "dedup:reject"),
		StructurizerCoverage:   res.StructurizerCoverage,
		ExtractorLatency:       latency,
		ExtractorTokenUsage:    res.ExtractorTokenUsage,
		ExtractorGuard:         res.ExtractorGuard,
		TierApplied:            ingest.TierAppliedFor(state.Tier),
		RecentMessagesProvided: len(state.RecentMessages),
		AnchorsProvided:        len(state.ExistingFactsAnchor),
		Dropped:                droppedFactsForTelemetry(state, res.Dropped),
		KnownEntitiesSeen:      len(state.KnownEntities),
		FactStats:              computeFactStats(res.Facts),
	}
	if len(res.Facts) == 0 {
		return detail, pipeline.ShortCircuitWith("empty_ingest")
	}
	return detail, nil
}

// countDroppedReason tallies dropped facts whose Reason matches any
// of the supplied keys. Used by IngestDetail to surface policy /
// validation / dedup attribution without expecting the slice to be
// pre-bucketed.
func countDroppedReason(drops []diagnostic.DroppedFact, keys ...string) int {
	if len(drops) == 0 || len(keys) == 0 {
		return 0
	}
	count := 0
	for _, d := range drops {
		if slices.Contains(keys, d.Reason) {
			count++
		}
	}
	return count
}

var _ pipeline.Stage[*write.WriteState] = (*Ingest)(nil)
