package recall

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	retrievallens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/sideeffect"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func allocateSaveOutboxID(state *write.WriteState) {
	if state == nil || state.SaveOutboxID != "" {
		return
	}
	if state.AsyncRequestID != "" {
		state.SaveOutboxID = state.AsyncRequestID
		return
	}
	state.SaveOutboxID = fmt.Sprintf("save-%d", time.Now().UnixNano())
}

func (m *memory) sideEffectExecutor() *sideeffect.Executor {
	return &sideeffect.Executor{
		Fanout:      m.fanout,
		Projections: m.projections,
		Evolution:   m.evolution,
		Retrieval:   findRetrievalProjection(m.projections),
		Telemetry:   m.telemetry,
	}
}

func findRetrievalProjection(projections []port.Projection) *retrievallens.Projection {
	for _, p := range projections {
		if rp, ok := p.(*retrievallens.Projection); ok {
			return rp
		}
	}
	return nil
}

// SideEffectProcessor drains commit-after projection / embedding /
// evolution jobs. Save only enqueues these jobs; callers run this
// processor from their own worker loop.
type SideEffectProcessor interface {
	ProcessSideEffects(ctx context.Context, opts SideEffectProcessOptions) (SideEffectProcessResult, error)
}

type SideEffectProcessOptions struct {
	WorkerID string
	Scope    Scope
	Limit    int
	Now      time.Time
}

type SideEffectProcessResult struct {
	Claimed    int
	Completed  int
	Failed     int
	DeadLetter int
}

type SideEffectOutboxObserver interface {
	SideEffectOutboxStats(ctx context.Context, scope Scope) (SideEffectOutboxStats, error)
}

type SideEffectOutboxStats = port.SideEffectStats

func NewSideEffectProcessor(mem Memory) (SideEffectProcessor, bool) {
	m, ok := mem.(*memory)
	if !ok || m == nil || m.sideEffectOutbox == nil {
		return nil, false
	}
	return m, true
}

func (m *memory) SideEffectOutboxStats(ctx context.Context, scope Scope) (SideEffectOutboxStats, error) {
	if m.sideEffectOutbox == nil {
		return SideEffectOutboxStats{}, errdefs.Validationf(
			"recall.SideEffectOutboxStats: side-effect outbox not configured")
	}
	if scope.PartitionKey() == "" {
		return SideEffectOutboxStats{}, errdefs.Validationf(
			"recall.SideEffectOutboxStats: scope partition is required (RuntimeID and UserID)")
	}
	return m.sideEffectOutbox.Stats(ctx, scope, time.Now())
}

func (m *memory) ProcessSideEffects(ctx context.Context, opts SideEffectProcessOptions) (SideEffectProcessResult, error) {
	if m.sideEffectOutbox == nil {
		return SideEffectProcessResult{}, errdefs.Validationf(
			"recall.ProcessSideEffects: side-effect outbox not configured")
	}
	if opts.Scope.PartitionKey() == "" {
		return SideEffectProcessResult{}, errdefs.Validationf(
			"recall.ProcessSideEffects: scope partition is required (RuntimeID and UserID)")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 1
	}
	jobs, err := m.sideEffectOutbox.Claim(ctx, port.SideEffectClaimOptions{
		WorkerID: opts.WorkerID,
		Scope:    opts.Scope,
		Max:      limit,
		Now:      now,
	})
	if err != nil {
		return SideEffectProcessResult{}, err
	}
	res := SideEffectProcessResult{Claimed: len(jobs)}
	exec := m.sideEffectExecutor()
	for _, job := range jobs {
		if err := exec.Run(ctx, job); err != nil {
			res.Failed++
			failure := sideEffectFailure(err, job.Attempt, now)
			if failure.ErrClass == diagnostic.ErrClassPermanent {
				res.DeadLetter++
			}
			if ackErr := m.sideEffectOutbox.Fail(ctx, job.ID, job.LeaseToken, failure); ackErr != nil {
				return res, fmt.Errorf("recall.ProcessSideEffects: fail ack %s: %w", job.ID, ackErr)
			}
			continue
		}
		if err := m.sideEffectOutbox.Complete(ctx, job.ID, job.LeaseToken, port.SideEffectResult{CompletedAt: now}); err != nil {
			res.Failed++
			return res, fmt.Errorf("recall.ProcessSideEffects: complete ack %s: %w", job.ID, err)
		}
		res.Completed++
	}
	return res, nil
}

const maxSideEffectAttempts = 5

func sideEffectFailure(err error, attempt int, now time.Time) port.SideEffectFailure {
	failure := port.SideEffectFailure{
		Err:      err.Error(),
		ErrClass: diagnostic.ErrClassTransient,
		RetryAt:  now.Add(sideEffectRetryBackoff(attempt)),
	}
	if attempt >= maxSideEffectAttempts {
		failure.ErrClass = diagnostic.ErrClassPermanent
		failure.RetryAt = time.Time{}
	}
	return failure
}

func sideEffectRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func (m *memory) purgeSideEffectOutbox(ctx context.Context, scope Scope) (int, error) {
	if m.sideEffectOutbox == nil {
		return 0, nil
	}
	return m.sideEffectOutbox.PurgeScope(ctx, scope)
}
