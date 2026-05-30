package stages

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// AppendEpisode writes the raw KindEpisode facts produced by
// build_episode to the canonical store. It is separate from Append so
// the runner / trace clearly distinguishes "raw episode lane" from the
// sync semantic append, and so the compensator can target the
// EpisodeFacts slice alone instead of state.Resolution.Facts (which
// the async lane never populates).
type AppendEpisode struct {
	store port.TemporalStore
	hook  port.TelemetryHook
}

// NewAppendEpisode constructs an AppendEpisode stage.
func NewAppendEpisode(store port.TemporalStore, hook port.TelemetryHook) *AppendEpisode {
	return &AppendEpisode{store: store, hook: hook}
}

// Name implements pipeline.Stage.
func (AppendEpisode) Name() string { return "append_episode" }

// Run implements pipeline.Stage. Episode facts go through the same
// store.Append boundary as semantic facts so canonical visibility,
// scope sharding, and ledger durability stay identical between the
// two lanes.
func (s *AppendEpisode) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	facts := state.EpisodeFacts
	started := time.Now()
	if err := s.store.Append(ctx, facts); err != nil {
		state.FailedStage = "append_episode"
		return diagnostic.AppendDetail{
			Facts:        len(facts),
			StoreLatency: time.Since(started),
		}, fmt.Errorf("recall.Save: append episode: %w", err)
	}
	return diagnostic.AppendDetail{
		Facts:        len(facts),
		StoreLatency: time.Since(started),
	}, nil
}

// Compensate implements pipeline.Compensator. It deletes exactly the
// episode fact IDs this stage wrote, so a downstream failure
// (project_episode_evidence / write_semantic_outbox) cannot leave a
// raw episode dangling without a queued semantic job.
func (s *AppendEpisode) Compensate(ctx context.Context, state *write.WriteState) error {
	ids := episodeFactIDs(state)
	if len(ids) == 0 {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if err := s.store.Delete(cleanupCtx, state.Scope, ids); err != nil {
		s.emitCompensationFailure(state.FailedStage, err)
	}
	return nil
}

// episodeFactIDs lifts the IDs from state.EpisodeFacts. We keep the
// helper separate from state.AppendedFactIDs (which may also include
// sync semantic IDs in mixed-mode futures) so the compensator never
// over-deletes.
func episodeFactIDs(state *write.WriteState) []string {
	if len(state.EpisodeFacts) == 0 {
		return nil
	}
	out := make([]string, 0, len(state.EpisodeFacts))
	for _, f := range state.EpisodeFacts {
		if f.ID == "" {
			continue
		}
		out = append(out, f.ID)
	}
	return out
}

func (s *AppendEpisode) emitCompensationFailure(failedStage string, err error) {
	if s.hook == nil {
		return
	}
	now := time.Now()
	s.hook.OnStage(diagnostic.StageDiagnostic{
		Stage:    "append_episode:compensate",
		Phase:    diagnostic.PhaseWrite,
		StartAt:  now,
		Duration: 0,
		Status:   diagnostic.StatusFailed,
		Err:      err.Error(),
		Detail: diagnostic.CompensationFailedDetail{
			OriginalStage: "save_rollback.episode_delete",
			Cause:         failedStage,
		},
	})
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*AppendEpisode)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*AppendEpisode)(nil)
)
