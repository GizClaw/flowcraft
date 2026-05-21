package recall

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/forget"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	writestages "github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// AsyncSemanticProcessor is the caller-driven drain entry for queued
// semantic extraction (F.1b). It is intentionally separate from Memory
// so the main facade does not own goroutine lifecycles.
type AsyncSemanticProcessor interface {
	ProcessAsyncSemantic(ctx context.Context, opts AsyncSemanticProcessOptions) (AsyncSemanticProcessResult, error)
}

// AsyncSemanticProcessOptions controls one ProcessAsyncSemantic batch.
type AsyncSemanticProcessOptions struct {
	WorkerID string
	Limit    int
	Now      time.Time
	// Scope, when non-zero, restricts Claim to that partition.
	Scope Scope
	// RuntimeID restricts Claim to jobs for one runtime when Scope is
	// zero. Ignored when Scope.RuntimeID is set.
	RuntimeID string
}

// AsyncSemanticProcessResult summarizes one drain pass.
type AsyncSemanticProcessResult struct {
	Claimed   int
	Completed int
	Recovered int
	Failed    int
}

// NewAsyncSemanticProcessor returns a processor when mem is a recall
// Memory wired with WithAsyncSemanticQueue. The bool is false when
// the queue is not configured.
func NewAsyncSemanticProcessor(mem Memory) (AsyncSemanticProcessor, bool) {
	m, ok := mem.(*memory)
	if !ok || m == nil || m.asyncSemanticQueue == nil {
		return nil, false
	}
	return m, true
}

// AsyncSemanticQueueObserver exposes queue health when Memory is wired
// with WithAsyncSemanticQueue and the backend implements Stats.
type AsyncSemanticQueueObserver interface {
	AsyncSemanticQueueStats(ctx context.Context, scope Scope) (AsyncSemanticQueueStats, error)
}

// AsyncSemanticQueueStats is the operator-facing queue snapshot (F.1c).
type AsyncSemanticQueueStats = port.AsyncSemanticStats

// AsyncSemanticQueueStats returns queue depth and terminal counts.
func (m *memory) AsyncSemanticQueueStats(ctx context.Context, scope Scope) (AsyncSemanticQueueStats, error) {
	if m.asyncSemanticQueue == nil {
		return AsyncSemanticQueueStats{}, errdefs.Validationf(
			"recall.AsyncSemanticQueueStats: requires WithAsyncSemanticQueue")
	}
	if scope.CanonicalKey() == "" {
		return AsyncSemanticQueueStats{}, errdefs.Validationf(
			"recall.AsyncSemanticQueueStats: scope partition is required (RuntimeID and UserID)")
	}
	return m.asyncSemanticQueue.Stats(ctx, port.AsyncSemanticStatsFilter{Scope: scope})
}

// ProcessAsyncSemantic claims and processes up to opts.Limit jobs.
func (m *memory) ProcessAsyncSemantic(ctx context.Context, opts AsyncSemanticProcessOptions) (AsyncSemanticProcessResult, error) {
	if m.asyncSemanticQueue == nil {
		return AsyncSemanticProcessResult{}, errdefs.Validationf(
			"recall.ProcessAsyncSemantic: requires WithAsyncSemanticQueue")
	}
	if err := ctx.Err(); err != nil {
		return AsyncSemanticProcessResult{}, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 1
	}
	workerID := opts.WorkerID
	if workerID == "" {
		workerID = "async-semantic-worker"
	}

	claimOpts := port.AsyncSemanticClaimOptions{
		WorkerID:  workerID,
		Now:       now,
		Max:       limit,
		RuntimeID: opts.RuntimeID,
	}
	if opts.Scope.RuntimeID != "" || opts.Scope.UserID != "" {
		scope := opts.Scope
		claimOpts.Scope = &scope
	}
	if claimOpts.Scope == nil && claimOpts.RuntimeID == "" {
		return AsyncSemanticProcessResult{}, errdefs.Validationf(
			"recall.ProcessAsyncSemantic: Scope or RuntimeID is required")
	}

	jobs, err := m.asyncSemanticQueue.Claim(ctx, claimOpts)
	if err != nil {
		return AsyncSemanticProcessResult{}, err
	}
	var res AsyncSemanticProcessResult
	res.Claimed = len(jobs)
	for _, job := range jobs {
		outcome := m.processOneAsyncSemanticJob(ctx, job, now)
		switch outcome {
		case asyncJobCompleted:
			res.Completed++
		case asyncJobRecovered:
			res.Recovered++
			res.Completed++
		case asyncJobFailed:
			res.Failed++
		}
	}
	return res, nil
}

type asyncJobOutcome int

const (
	asyncJobCompleted asyncJobOutcome = iota
	asyncJobRecovered
	asyncJobFailed
)

func (m *memory) processOneAsyncSemanticJob(ctx context.Context, job port.AsyncSemanticJob, now time.Time) asyncJobOutcome {
	start := time.Now()

	if recovered, ids := m.recoverCompletedSemanticFacts(ctx, job, now); recovered {
		_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{
			SemanticFactIDs:           ids,
			RecoveredFromPriorAttempt: true,
		})
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID:  job.RequestID,
			Attempt:         job.Attempt,
			SemanticFactIDs: append([]string(nil), ids...),
			Recovered:       true,
		}, diagnostic.StatusOK, "", start)
		return asyncJobRecovered
	}

	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		_ = m.asyncSemanticQueue.Fail(ctx, job.RequestID, port.AsyncSemanticFailure{
			ErrClass: diagnostic.ErrClassPermanent,
			Err:      err.Error(),
		})
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}

	turns, err := writestages.ReconstructTurnsForJob(ctx, m.store, job)
	if err != nil {
		_ = m.asyncSemanticQueue.Fail(ctx, job.RequestID, port.AsyncSemanticFailure{
			ErrClass: diagnostic.ErrClassPermanent,
			Err:      err.Error(),
		})
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}

	state := &write.WriteState{
		Scope:               job.Scope,
		Turns:               turns,
		ObservedAt:          job.ObservedAt,
		Tier:                job.Tier,
		RecentMessages:      job.RecentMessages,
		ExistingFactsAnchor: job.ExistingFactsAnchor,
		Now:                 now,
		AsyncRequestID:      job.RequestID,
		SemanticDerivationOrigin: domain.FactOrigin{
			RequestID:      job.RequestID,
			Kind:           domain.OriginKindSemanticDerivation,
			EpisodeFactIDs: append([]string(nil), job.EpisodeFactIDs...),
		},
	}
	state.EnsureTrace()

	// Ingest (possibly LLM-backed) runs outside the scope write lock,
	// matching the sync Save path and avoiding blocking other writers.
	if err := m.asyncSemanticWorkerPreRunner.Run(ctx, state); err != nil {
		m.failAsyncJob(ctx, job.RequestID, err)
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}

	unlock := m.lockWriteScope(job.Scope)
	defer unlock()

	// Re-validate under the lock so ForgetAll / ExpireRetired cannot
	// delete episodes after the lock-free preflight.
	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		m.failAsyncJob(ctx, job.RequestID, err)
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}

	// Re-check origin under the lock: another worker may have appended
	// semantic facts while this worker held an expired lease and ran
	// ingest outside the lock.
	if recovered, ids := m.recoverCompletedSemanticFacts(ctx, job, now); recovered {
		_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{
			SemanticFactIDs:           ids,
			RecoveredFromPriorAttempt: true,
		})
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID:  job.RequestID,
			Attempt:         job.Attempt,
			SemanticFactIDs: append([]string(nil), ids...),
			Recovered:       true,
		}, diagnostic.StatusOK, "", start)
		return asyncJobRecovered
	}

	if len(state.Ingest.Facts) == 0 {
		_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{})
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusOK, "", start)
		return asyncJobCompleted
	}
	if err := m.asyncSemanticWorkerPostRunner.Run(ctx, state); err != nil {
		m.failAsyncJob(ctx, job.RequestID, err)
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}
	ids := append([]string(nil), state.AppendedFactIDs...)
	_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{
		SemanticFactIDs: ids,
	})
	m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
		AsyncRequestID:  job.RequestID,
		Attempt:         job.Attempt,
		SemanticFactIDs: ids,
	}, diagnostic.StatusOK, "", start)
	return asyncJobCompleted
}

func (m *memory) emitAsyncProcessStage(
	job port.AsyncSemanticJob,
	detail diagnostic.AsyncSemanticProcessDetail,
	status diagnostic.Status,
	errMsg string,
	start time.Time,
) {
	if m.telemetry == nil {
		return
	}
	m.telemetry.OnStage(diagnostic.StageDiagnostic{
		Stage:          "process_async_semantic",
		Phase:          diagnostic.PhaseWrite,
		StartAt:        start,
		Duration:       time.Since(start),
		Status:         status,
		Err:            errMsg,
		AsyncRequestID: job.RequestID,
		Detail:         detail,
	})
}

func (m *memory) recoverCompletedSemanticFacts(ctx context.Context, job port.AsyncSemanticJob, now time.Time) (bool, []string) {
	existing, err := m.store.FindByOriginRequestID(ctx, job.Scope, job.RequestID)
	if err != nil || len(existing) == 0 {
		return false, nil
	}
	var ids []string
	for _, f := range existing {
		if f.Origin.Kind != domain.OriginKindSemanticDerivation {
			continue
		}
		if !domain.IsCanonicalActive(f, now) {
			continue
		}
		ids = append(ids, f.ID)
	}
	if len(ids) == 0 {
		return false, nil
	}
	return true, ids
}

func (m *memory) failAsyncJob(ctx context.Context, requestID string, err error) {
	class := diagnostic.ErrClassTransient
	if errdefs.IsValidation(err) {
		class = diagnostic.ErrClassPermanent
	}
	var retryAt time.Time
	if class == diagnostic.ErrClassTransient {
		retryAt = time.Now().Add(defaultAsyncSemanticRetryBackoff)
	}
	_ = m.asyncSemanticQueue.Fail(ctx, requestID, port.AsyncSemanticFailure{
		ErrClass: class,
		Err:      err.Error(),
		RetryAt:  retryAt,
	})
}

const defaultAsyncSemanticRetryBackoff = 30 * time.Second

type asyncJobCancelResult struct {
	Cancelled int
	Err       error
}

func (m *memory) cancelAsyncJobsAfterForget(ctx context.Context, state *forget.State) asyncJobCancelResult {
	if m.asyncSemanticQueue == nil || state == nil {
		return asyncJobCancelResult{}
	}
	if state.Filter == nil && domain.NormalizeForgetMode(state.Mode) == domain.ForgetHard {
		n, err := m.asyncSemanticQueue.CancelScope(ctx, state.Scope)
		return asyncJobCancelResult{Cancelled: n, Err: err}
	}
	if len(state.DeletedFactIDs) == 0 {
		return asyncJobCancelResult{}
	}
	n, err := m.asyncSemanticQueue.CancelMatchingEpisodes(ctx, state.Scope, state.DeletedFactIDs)
	return asyncJobCancelResult{Cancelled: n, Err: err}
}

func (m *memory) patchForgetTraceAsyncCancel(state *forget.State, cancel asyncJobCancelResult) {
	if state == nil || state.Trace == nil || len(state.Trace.Stages) == 0 {
		return
	}
	last := &state.Trace.Stages[len(state.Trace.Stages)-1]
	switch d := last.Detail.(type) {
	case diagnostic.ForgetAllDetail:
		d.AsyncJobsCancelled = cancel.Cancelled
		if cancel.Err != nil {
			d.AsyncJobCancelErr = cancel.Err.Error()
		}
		last.Detail = d
	case diagnostic.ExpireRetiredDetail:
		d.AsyncJobsCancelled = cancel.Cancelled
		if cancel.Err != nil {
			d.AsyncJobCancelErr = cancel.Err.Error()
		}
		last.Detail = d
	}
}

func (m *memory) emitAsyncJobCancelTelemetry(scope Scope, cancel asyncJobCancelResult, stage string) {
	if m.telemetry == nil || cancel.Err == nil {
		return
	}
	now := time.Now()
	m.telemetry.OnStage(diagnostic.StageDiagnostic{
		Stage:    stage + ":async_job_cancel",
		Phase:    diagnostic.PhaseWrite,
		StartAt:  now,
		Duration: 0,
		Status:   diagnostic.StatusFailed,
		Err:      cancel.Err.Error(),
		Detail: diagnostic.CompensationFailedDetail{
			OriginalStage: "async_job_cancel",
			Cause:         fmt.Sprintf("cancelled=%d", cancel.Cancelled),
		},
	})
	_ = scope
}
