package recall

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/forget"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	writestages "github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// AsyncSemanticProcessor is the caller-driven drain entry for queued
// semantic extraction (F.1b). It is intentionally separate from Memory
// so the main facade does not own goroutine lifecycles. The core does not
// start, stop, drain, or close async semantic workers.
type AsyncSemanticProcessor interface {
	ProcessAsyncSemantic(ctx context.Context, opts AsyncSemanticProcessOptions) (AsyncSemanticProcessResult, error)
}

// AsyncSemanticProcessOptions controls one ProcessAsyncSemantic batch.
type AsyncSemanticProcessOptions struct {
	WorkerID string
	Limit    int
	Now      time.Time
	// Scope restricts Claim to that partition and is required.
	Scope Scope
	// RuntimeID is retained for source compatibility but is not
	// accepted by ProcessAsyncSemantic. Runtime-wide draining must go
	// through an explicit privileged/admin entry point.
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
	if scope.PartitionKey() == "" {
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

	if opts.Scope.PartitionKey() == "" {
		return AsyncSemanticProcessResult{}, errdefs.Validationf(
			"recall.ProcessAsyncSemantic: scope partition is required (RuntimeID and UserID)")
	}
	if opts.RuntimeID != "" {
		return AsyncSemanticProcessResult{}, errdefs.Validationf(
			"recall.ProcessAsyncSemantic: RuntimeID-only drain is not supported; pass Scope")
	}
	scope := opts.Scope
	claimOpts := port.AsyncSemanticClaimOptions{
		WorkerID: workerID,
		Now:      now,
		Max:      limit,
		Scope:    &scope,
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
		if err := m.completeAsyncJob(ctx, job, port.AsyncSemanticResult{
			SemanticFactIDs:           ids,
			RecoveredFromPriorAttempt: true,
		}); err != nil {
			m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
				AsyncRequestID: job.RequestID,
				Attempt:        job.Attempt,
			}, diagnostic.StatusFailed, err.Error(), start)
			return asyncJobFailed
		}
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID:  job.RequestID,
			Attempt:         job.Attempt,
			SemanticFactIDs: append([]string(nil), ids...),
			Recovered:       true,
		}, diagnostic.StatusOK, "", start)
		return asyncJobRecovered
	}

	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}

	turns, err := writestages.ReconstructTurnsForJob(ctx, m.store, job)
	if err != nil {
		m.ackAsyncJobFail(ctx, job, start, err)
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

	startGen := m.peekScopeGen(job.Scope)
	// Ingest (possibly LLM-backed) runs outside the scope write lock,
	// matching the sync Save path and avoiding blocking other writers.
	if err := m.asyncSemanticWorkerPreRunner.Run(ctx, state); err != nil {
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}

	m.holdWriteTelemetry()
	unlock, err := m.enterScopeWrite(job.Scope, startGen)
	if err != nil {
		m.flushWriteTelemetry()
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}
	allocateSaveOutboxID(state)

	// Re-validate under the lock so ForgetAll / ExpireRetired cannot
	// delete episodes after the lock-free preflight.
	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		unlock()
		m.flushWriteTelemetry()
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}

	// Re-check origin under the lock: another worker may have appended
	// semantic facts while this worker held an expired lease and ran
	// ingest outside the lock.
	if recovered, ids := m.recoverCompletedSemanticFacts(ctx, job, now); recovered {
		unlock()
		m.flushWriteTelemetry()
		if err := m.completeAsyncJob(ctx, job, port.AsyncSemanticResult{
			SemanticFactIDs:           ids,
			RecoveredFromPriorAttempt: true,
		}); err != nil {
			m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
				AsyncRequestID: job.RequestID,
				Attempt:        job.Attempt,
			}, diagnostic.StatusFailed, err.Error(), start)
			return asyncJobFailed
		}
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID:  job.RequestID,
			Attempt:         job.Attempt,
			SemanticFactIDs: append([]string(nil), ids...),
			Recovered:       true,
		}, diagnostic.StatusOK, "", start)
		return asyncJobRecovered
	}

	if len(state.Ingest.Facts) == 0 {
		unlock()
		m.flushWriteTelemetry()
		if err := m.completeAsyncJob(ctx, job, port.AsyncSemanticResult{}); err != nil {
			m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
				AsyncRequestID: job.RequestID,
				Attempt:        job.Attempt,
			}, diagnostic.StatusFailed, err.Error(), start)
			return asyncJobFailed
		}
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusOK, "", start)
		return asyncJobCompleted
	}
	if err := m.asyncSemanticWorkerPostRunner.Run(ctx, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}
	if err := m.abortIfScopeGenChanged(job.Scope, startGen, state); err != nil {
		unlock()
		m.flushWriteTelemetry()
		m.ackAsyncJobFail(ctx, job, start, err)
		return asyncJobFailed
	}
	unlock()
	m.flushWriteTelemetry()
	ids := append([]string(nil), state.AppendedFactIDs...)
	if err := m.completeAsyncJob(ctx, job, port.AsyncSemanticResult{
		SemanticFactIDs: ids,
	}); err != nil {
		m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
			AsyncRequestID: job.RequestID,
			Attempt:        job.Attempt,
		}, diagnostic.StatusFailed, err.Error(), start)
		return asyncJobFailed
	}
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

func (m *memory) completeAsyncJob(ctx context.Context, job port.AsyncSemanticJob, result port.AsyncSemanticResult) error {
	return m.asyncSemanticQueue.Complete(ctx, job.RequestID, job.LeaseToken, result)
}

func (m *memory) failAsyncJob(ctx context.Context, job port.AsyncSemanticJob, err error) error {
	class := diagnostic.ErrClassTransient
	if errdefs.IsValidation(err) {
		class = diagnostic.ErrClassPermanent
	}
	var retryAt time.Time
	if class == diagnostic.ErrClassTransient {
		retryAt = time.Now().Add(defaultAsyncSemanticRetryBackoff)
	}
	return m.asyncSemanticQueue.Fail(ctx, job.RequestID, job.LeaseToken, port.AsyncSemanticFailure{
		ErrClass: class,
		Err:      err.Error(),
		RetryAt:  retryAt,
	})
}

func (m *memory) ackAsyncJobFail(ctx context.Context, job port.AsyncSemanticJob, start time.Time, cause error) {
	errMsg := cause.Error()
	if ackErr := m.failAsyncJob(ctx, job, cause); ackErr != nil {
		errMsg = fmt.Sprintf("%s; queue fail: %v", cause.Error(), ackErr)
	}
	m.emitAsyncProcessStage(job, diagnostic.AsyncSemanticProcessDetail{
		AsyncRequestID: job.RequestID,
		Attempt:        job.Attempt,
	}, diagnostic.StatusFailed, errMsg, start)
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
		if err != nil {
			return asyncJobCancelResult{Cancelled: n, Err: err}
		}
		purged, purgeErr := m.asyncSemanticQueue.PurgeScope(ctx, state.Scope)
		if purgeErr != nil {
			return asyncJobCancelResult{Cancelled: n, Err: purgeErr}
		}
		sidePurged, sideErr := m.purgeSideEffectOutbox(ctx, state.Scope)
		if sideErr != nil {
			return asyncJobCancelResult{Cancelled: n + purged, Err: sideErr}
		}
		return asyncJobCancelResult{Cancelled: n + purged + sidePurged, Err: nil}
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

func (m *memory) emitAsyncJobCancelTelemetry(state *forget.State, cancel asyncJobCancelResult, stage string) {
	if m.telemetry == nil {
		return
	}
	if cancel.Cancelled == 0 && cancel.Err == nil {
		return
	}
	if state == nil || state.Trace == nil || len(state.Trace.Stages) == 0 {
		m.emitAsyncJobCancelTelemetryFallback(cancel, stage)
		return
	}
	last := state.Trace.Stages[len(state.Trace.Stages)-1]
	status := diagnostic.StatusOK
	errMsg := ""
	if cancel.Err != nil {
		status = diagnostic.StatusFailed
		errMsg = cancel.Err.Error()
	}
	m.telemetry.OnStage(diagnostic.StageDiagnostic{
		Stage:    stage,
		Phase:    diagnostic.PhaseWrite,
		StartAt:  last.StartAt,
		Duration: last.Duration,
		Status:   status,
		Err:      errMsg,
		Detail:   last.Detail,
	})
}

func (m *memory) emitAsyncJobCancelTelemetryFallback(cancel asyncJobCancelResult, stage string) {
	now := time.Now()
	status := diagnostic.StatusOK
	errMsg := ""
	if cancel.Err != nil {
		status = diagnostic.StatusFailed
		errMsg = cancel.Err.Error()
	}
	m.telemetry.OnStage(diagnostic.StageDiagnostic{
		Stage:    stage + ":async_job_cancel",
		Phase:    diagnostic.PhaseWrite,
		StartAt:  now,
		Duration: 0,
		Status:   status,
		Err:      errMsg,
		Detail: diagnostic.CompensationFailedDetail{
			OriginalStage: "async_job_cancel",
			Cause:         fmt.Sprintf("cancelled=%d", cancel.Cancelled),
		},
	})
}
