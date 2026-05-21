package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// StructuredIngest is the structured-facts leg of WriteModeAsyncSemantic.
// It runs AFTER the episode lane (build → append → evidence → outbox)
// so a failure here triggers framework compensation that cancels the
// outbox job and rolls back the raw episode. Unlike the sync Ingest
// stage, Turns are deliberately omitted — caller Turns were already
// captured as KindEpisode facts upstream.
type StructuredIngest struct {
	ingestor port.Ingestor
	snapshot EntitySnapshotFunc
}

// NewStructuredIngest constructs the stage.
func NewStructuredIngest(ingestor port.Ingestor, snapshot EntitySnapshotFunc) *StructuredIngest {
	return &StructuredIngest{ingestor: ingestor, snapshot: snapshot}
}

// Name implements pipeline.Stage.
func (StructuredIngest) Name() string { return "structured_ingest" }

// Skip implements pipeline.Conditional. Turns-only async saves have
// no caller-supplied Facts; the episode lane alone satisfies the ack.
func (StructuredIngest) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if len(state.Facts) == 0 {
		return true, diagnostic.IngestDetail{}
	}
	return false, nil
}

// Run implements pipeline.Stage. Empty compile output is a normal
// outcome (policy drops everything) — we do NOT ShortCircuit because
// the episode lane already committed; downstream resolve Skip handles
// the empty work set.
func (s *StructuredIngest) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	if s.snapshot != nil {
		state.KnownEntities = s.snapshot(state.Scope)
	}
	started := time.Now()
	res, err := s.ingestor.Compile(ctx, port.IngestInput{
		Scope:               state.Scope,
		Facts:               state.Facts,
		Turns:               nil,
		ObservedAt:          state.ObservedAt,
		KnownEntities:       state.KnownEntities,
		Now:                 state.Now,
		Tier:                state.Tier,
		RecentMessages:      state.RecentMessages,
		ExistingFactsAnchor: state.ExistingFactsAnchor,
	})
	latency := time.Since(started)
	if err != nil {
		state.FailedStage = "structured_ingest"
		return diagnostic.IngestDetail{
			InputTurns:       0,
			ExtractedFacts:   len(res.Facts),
			ExtractorLatency: latency,
		}, err
	}
	state.Ingest = res
	return diagnostic.IngestDetail{
		InputTurns:             0,
		ExtractedFacts:         len(res.Facts),
		DroppedByPolicy:        countDroppedReason(res.Dropped, "policy:reject", "governance:reject"),
		DroppedByValidation:    countDroppedReason(res.Dropped, "validation:reject"),
		DroppedByDedup:         countDroppedReason(res.Dropped, "dedup:reject"),
		StructurizerCoverage:   res.StructurizerCoverage,
		ExtractorLatency:       latency,
		TierApplied:            ingest.TierAppliedFor(state.Tier),
		RecentMessagesProvided: len(state.RecentMessages),
		AnchorsProvided:        len(state.ExistingFactsAnchor),
		Dropped:                append([]diagnostic.DroppedFact(nil), res.Dropped...),
		KnownEntitiesSeen:      len(state.KnownEntities),
	}, nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*StructuredIngest)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*StructuredIngest)(nil)
)

// asyncStructuredLegInactive reports whether the structured-facts leg
// has no work. Shared by resolve / append / validity / projection
// stages in the async episode runner (before write_semantic_outbox).
func asyncStructuredLegInactive(state *write.WriteState) bool {
	if state == nil || state.Mode != domain.WriteModeAsyncSemantic {
		return false
	}
	if len(state.Facts) == 0 {
		return true
	}
	return len(state.Ingest.Facts) == 0 && len(state.Resolution.Facts) == 0 && len(state.Resolution.Closes) == 0
}
