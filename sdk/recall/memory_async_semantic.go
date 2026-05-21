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
	if recovered, ids := m.recoverCompletedSemanticFacts(ctx, job, now); recovered {
		_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{
			SemanticFactIDs:           ids,
			RecoveredFromPriorAttempt: true,
		})
		return asyncJobRecovered
	}

	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		_ = m.asyncSemanticQueue.Fail(ctx, job.RequestID, port.AsyncSemanticFailure{
			ErrClass: diagnostic.ErrClassPermanent,
			Err:      err.Error(),
		})
		return asyncJobFailed
	}

	turns, err := writestages.ReconstructTurnsForJob(ctx, m.store, job)
	if err != nil {
		_ = m.asyncSemanticQueue.Fail(ctx, job.RequestID, port.AsyncSemanticFailure{
			ErrClass: diagnostic.ErrClassPermanent,
			Err:      err.Error(),
		})
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
		return asyncJobFailed
	}

	unlock := m.lockWriteScope(job.Scope)
	defer unlock()

	// Re-validate under the lock so ForgetAll / ExpireRetired cannot
	// delete episodes after the lock-free preflight.
	if err := writestages.ValidateEpisodesForJob(ctx, m.store, job, now); err != nil {
		m.failAsyncJob(ctx, job.RequestID, err)
		return asyncJobFailed
	}

	if len(state.Ingest.Facts) == 0 {
		_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{})
		return asyncJobCompleted
	}
	if err := m.asyncSemanticWorkerPostRunner.Run(ctx, state); err != nil {
		m.failAsyncJob(ctx, job.RequestID, err)
		return asyncJobFailed
	}
	_ = m.asyncSemanticQueue.Complete(ctx, job.RequestID, port.AsyncSemanticResult{
		SemanticFactIDs: append([]string(nil), state.AppendedFactIDs...),
	})
	return asyncJobCompleted
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
	_ = m.asyncSemanticQueue.Fail(ctx, requestID, port.AsyncSemanticFailure{
		ErrClass: class,
		Err:      err.Error(),
	})
}

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
