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

// WriteSemanticOutbox is the durable boundary of the async save lane.
// It runs after the structured-facts leg and project_required so a job
// is not claimable until episode + optional structured work succeed.
// On Enqueue failure the framework reverse-walks upstream stages
// (including append / project_episode_evidence) via Compensator.
type WriteSemanticOutbox struct {
	queue port.AsyncSemanticQueue
	hook  port.TelemetryHook
}

// NewWriteSemanticOutbox constructs the stage. queue must be non-nil
// when the stage is registered; the memory facade guards against the
// nil case by short-circuiting Save with a validation error before
// the runner is invoked.
func NewWriteSemanticOutbox(queue port.AsyncSemanticQueue, hook port.TelemetryHook) *WriteSemanticOutbox {
	return &WriteSemanticOutbox{queue: queue, hook: hook}
}

// Name implements pipeline.Stage.
func (WriteSemanticOutbox) Name() string { return "write_semantic_outbox" }

// Run implements pipeline.Stage. The Enqueue call is the
// scope-write-lock-bound durability boundary; remote queue backends
// MUST satisfy the §4.2 outbox SLA (local-durable, <10ms p99) so the
// lock is not held by remote latency.
func (s *WriteSemanticOutbox) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	ids := episodeFactIDs(state)
	job := port.AsyncSemanticJob{
		RequestID:           state.AsyncRequestID,
		Scope:               state.Scope,
		SaveOutboxID:        state.SaveOutboxID,
		EpisodeFactIDs:      ids,
		TurnsSnapshot:       state.Turns,
		SourceEvidenceSpans: state.SourceEvidenceSpans,
		ObservedAt:          state.ObservedAt,
		Tier:                state.Tier,
		RecentMessages:      state.RecentMessages,
		ExistingFactHints:   state.ExistingFactHints,
		EvidenceWindowRefs:  state.EvidenceWindowRefs,
	}
	if s.queue == nil {
		state.FailedStage = "write_semantic_outbox"
		return diagnostic.EnqueueSemanticDetail{
			AsyncRequestID: state.AsyncRequestID,
			EpisodeFactIDs: ids,
			Latency:        time.Since(started),
		}, fmt.Errorf("recall.Save: async semantic queue not configured")
	}
	if _, err := s.queue.Enqueue(ctx, port.CloneAsyncSemanticJob(job)); err != nil {
		state.FailedStage = "write_semantic_outbox"
		return diagnostic.EnqueueSemanticDetail{
			AsyncRequestID: state.AsyncRequestID,
			EpisodeFactIDs: ids,
			Latency:        time.Since(started),
		}, fmt.Errorf("recall.Save: outbox enqueue: %w", err)
	}
	state.SemanticPending = true
	return diagnostic.EnqueueSemanticDetail{
		AsyncRequestID: state.AsyncRequestID,
		EpisodeFactIDs: ids,
		Latency:        time.Since(started),
	}, nil
}

// Compensate implements pipeline.Compensator. When a downstream stage
// (structured facts) fails after a successful Enqueue, cancel the
// durable outbox record so workers never process a job whose Save
// caller observed as failed.
func (s *WriteSemanticOutbox) Compensate(ctx context.Context, state *write.WriteState) error {
	if !state.SemanticPending || s.queue == nil || state.AsyncRequestID == "" {
		return nil
	}
	cleanupCtx := pipeline.DetachCancel(ctx)
	if err := s.queue.Cancel(cleanupCtx, state.AsyncRequestID); err != nil && s.hook != nil {
		now := time.Now()
		s.hook.OnStage(diagnostic.StageDiagnostic{
			Stage:   "write_semantic_outbox:compensate",
			Phase:   diagnostic.PhaseWrite,
			StartAt: now,
			Status:  diagnostic.StatusFailed,
			Err:     err.Error(),
			Detail: diagnostic.CompensationFailedDetail{
				OriginalStage: "save_rollback.outbox_cancel",
				Cause:         state.FailedStage,
			},
		})
	}
	state.SemanticPending = false
	return nil
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*WriteSemanticOutbox)(nil)
	_ pipeline.Compensator[*write.WriteState] = (*WriteSemanticOutbox)(nil)
)
