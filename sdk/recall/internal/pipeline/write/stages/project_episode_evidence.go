package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// ProjectEpisodeEvidence drives the kind-filtered required-projection
// fanout for the episode lane. Today only the evidence projection
// accepts KindEpisode (see lens/evidence/projection.go); the other
// required projections (retrieval / entity / etc.) declare
// AcceptsKind=false so Fanout.ProjectRequiredForKindsStrict skips them.
//
// The compensator handles two failure cases:
//   - downstream (structured_ingest … write_semantic_outbox) failure: roll back the
//     evidence mirror by walking the same required-projection list
//     and Forget()ing the episode IDs from every projection that
//     accepts the kind. The fanout has no "rollback only what I
//     projected" helper because the original Project ran one
//     projection deep, so we mirror the iteration here.
//   - this stage's own failure: Run returns the error and the
//     framework does NOT call this stage's Compensator (failing
//     stages skip their own compensator), only earlier stages.
type ProjectEpisodeEvidence struct {
	fanout      *pipeline.Fanout
	projections []port.Projection
	hook        port.TelemetryHook
}

// NewProjectEpisodeEvidence constructs the stage. projections is the
// canonical projection list memory.New() compiled (in registration
// order) so the compensator can iterate the same set the fanout would.
// Either projections or fanout may be nil for narrow unit tests.
func NewProjectEpisodeEvidence(fanout *pipeline.Fanout, projections []port.Projection, hook port.TelemetryHook) *ProjectEpisodeEvidence {
	return &ProjectEpisodeEvidence{fanout: fanout, projections: projections, hook: hook}
}

// Name implements pipeline.Stage.
func (ProjectEpisodeEvidence) Name() string { return "project_episode_evidence" }

// Run implements pipeline.Stage.
func (s *ProjectEpisodeEvidence) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	facts := state.EpisodeFacts
	if s.fanout != nil && len(facts) > 0 {
		if err := s.fanout.ProjectRequiredForKindsStrict(ctx, facts, domain.KindEpisode); err != nil {
			state.FailedStage = "project_episode_evidence"
			return diagnostic.ProjectEpisodeEvidenceDetail{
				AsyncRequestID: state.AsyncRequestID,
				EpisodeFacts:   len(facts),
				Latency:        time.Since(started),
			}, err
		}
	}
	state.EvidenceAppliedEpisode = true
	return diagnostic.ProjectEpisodeEvidenceDetail{
		AsyncRequestID: state.AsyncRequestID,
		EpisodeFacts:   len(facts),
		Latency:        time.Since(started),
	}, nil
}

// Compensate implements pipeline.Compensator. Invoked when a stage
// AFTER project_episode_evidence fails. Walks every required projection that accepts KindEpisode and
// issues Forget on the episode IDs.
func (s *ProjectEpisodeEvidence) Compensate(ctx context.Context, state *write.WriteState) error {
	if !state.EvidenceAppliedEpisode {
		return nil
	}
	ids := episodeFactIDs(state)
	if len(ids) == 0 {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	for _, p := range s.projections {
		if p == nil {
			continue
		}
		if !pipeline.ProjectionAcceptsKindStrict(p, domain.KindEpisode) {
			continue
		}
		if err := p.Forget(cleanupCtx, state.Scope, ids); err != nil {
			s.emitCompensationFailure(p.Name(), state.FailedStage, err)
		}
	}
	return nil
}

func (s *ProjectEpisodeEvidence) emitCompensationFailure(projection, failedStage string, err error) {
	if s.hook == nil {
		return
	}
	now := time.Now()
	s.hook.OnStage(diagnostic.StageDiagnostic{
		Stage:    "project_episode_evidence:compensate",
		Phase:    diagnostic.PhaseWrite,
		StartAt:  now,
		Duration: 0,
		Status:   diagnostic.StatusFailed,
		Err:      err.Error(),
		Detail: diagnostic.CompensationFailedDetail{
			OriginalStage: "save_rollback.episode_evidence:" + projection,
			Cause:         failedStage,
		},
	})
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*ProjectEpisodeEvidence)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*ProjectEpisodeEvidence)(nil)
)
