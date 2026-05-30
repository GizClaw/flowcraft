package recall

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ReadinessStatus is the coarse operator-facing state for one readiness check.
type ReadinessStatus string

const (
	ReadinessReady    ReadinessStatus = "ready"
	ReadinessDegraded ReadinessStatus = "degraded"
	ReadinessNotReady ReadinessStatus = "not_ready"
	ReadinessSkipped  ReadinessStatus = "skipped"
)

// ReadinessObserver exposes a point-in-time operator checklist for one scope.
type ReadinessObserver interface {
	Readiness(ctx context.Context, scope Scope, opts ReadinessOptions) (ReadinessReport, error)
}

// ReadinessOptions controls how strict Readiness should be. Zero thresholds are
// intentionally conservative: any backlog, expired lease, or dead letter marks
// the relevant check degraded.
type ReadinessOptions struct {
	Now time.Time

	RequireAsyncSemantic          bool
	IncludeAsyncSemanticReconcile bool

	MaxSideEffectBacklog    int
	MaxAsyncSemanticBacklog int
	MaxExpiredLeases        int
	MaxDeadLetters          int
}

// ReadinessReport is the stable dashboard/checklist shape for recall core.
type ReadinessReport struct {
	Scope  Scope
	Status ReadinessStatus
	Checks []ReadinessCheck
}

// ReadinessCheck describes one subsystem's readiness contribution.
type ReadinessCheck struct {
	Name   string
	Status ReadinessStatus
	Reason string

	Pending        int
	Leased         int
	ExpiredLeases  int
	DeadLetter     int
	Backlog        int
	Completed      int
	CancelledTotal int

	CompletedSemantic     int
	PendingSemantic       int
	SkippedSemantic       int
	UnrecoverableSemantic int
}

func (m *memory) Readiness(ctx context.Context, scope Scope, opts ReadinessOptions) (ReadinessReport, error) {
	if err := ctx.Err(); err != nil {
		return ReadinessReport{}, err
	}
	if scope.PartitionKey() == "" {
		return ReadinessReport{}, errdefs.Validationf(
			"recall.Readiness: scope partition is required (RuntimeID and UserID)")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	report := ReadinessReport{
		Scope:  Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID},
		Status: ReadinessReady,
	}
	report.addCheck(m.readinessSideEffects(ctx, scope, opts))
	report.addCheck(m.readinessAsyncSemanticQueue(ctx, scope, opts))
	if opts.IncludeAsyncSemanticReconcile {
		report.addCheck(m.readinessAsyncSemanticReconcile(ctx, scope, opts))
	}
	return report, nil
}

func (m *memory) readinessSideEffects(ctx context.Context, scope Scope, opts ReadinessOptions) ReadinessCheck {
	check := ReadinessCheck{Name: "side_effect_outbox"}
	if m.sideEffectOutbox == nil {
		check.Status = ReadinessNotReady
		check.Reason = "side-effect outbox not configured"
		return check
	}
	stats, err := m.sideEffectOutbox.Stats(ctx, scope, opts.Now)
	if err != nil {
		check.Status = ReadinessNotReady
		check.Reason = err.Error()
		return check
	}
	check.Pending = stats.Pending
	check.Leased = stats.Leased
	check.ExpiredLeases = stats.ExpiredLeases
	check.DeadLetter = stats.DeadLetter
	check.Backlog = stats.Pending + stats.Leased
	check.Completed = stats.Completed
	check.CancelledTotal = stats.CancelledTotal
	check.Status, check.Reason = classifyQueueReadiness(
		check.Backlog,
		check.ExpiredLeases,
		check.DeadLetter,
		opts.MaxSideEffectBacklog,
		opts.MaxExpiredLeases,
		opts.MaxDeadLetters,
	)
	return check
}

func (m *memory) readinessAsyncSemanticQueue(ctx context.Context, scope Scope, opts ReadinessOptions) ReadinessCheck {
	check := ReadinessCheck{Name: "async_semantic_queue"}
	if m.asyncSemanticQueue == nil {
		if opts.RequireAsyncSemantic {
			check.Status = ReadinessNotReady
			check.Reason = "async semantic queue required but not configured"
			return check
		}
		check.Status = ReadinessSkipped
		check.Reason = "async semantic queue not configured"
		return check
	}
	stats, err := m.asyncSemanticQueue.Stats(ctx, AsyncSemanticStatsFilter{Scope: scope, Now: opts.Now})
	if err != nil {
		check.Status = ReadinessNotReady
		check.Reason = err.Error()
		return check
	}
	check.Pending = stats.Pending
	check.Leased = stats.Leased
	check.ExpiredLeases = stats.ExpiredLeases
	check.DeadLetter = stats.DeadLetter
	check.Backlog = stats.Pending + stats.Leased
	check.Completed = stats.Completed
	check.CancelledTotal = stats.CancelledTotal
	check.Status, check.Reason = classifyQueueReadiness(
		check.Backlog,
		check.ExpiredLeases,
		check.DeadLetter,
		opts.MaxAsyncSemanticBacklog,
		opts.MaxExpiredLeases,
		opts.MaxDeadLetters,
	)
	return check
}

func (m *memory) readinessAsyncSemanticReconcile(ctx context.Context, scope Scope, opts ReadinessOptions) ReadinessCheck {
	check := ReadinessCheck{Name: "async_semantic_reconcile"}
	res, err := m.ReconcileAsyncSemantic(ctx, scope, AsyncSemanticReconcileOptions{Now: opts.Now})
	if err != nil {
		check.Status = ReadinessNotReady
		check.Reason = err.Error()
		return check
	}
	check.CompletedSemantic = res.Completed
	check.PendingSemantic = res.Pending
	check.SkippedSemantic = res.Skipped
	check.UnrecoverableSemantic = res.Unrecoverable
	switch {
	case res.Unrecoverable > 0:
		check.Status = ReadinessDegraded
		check.Reason = "async semantic derivations include unrecoverable episodes"
	case res.Pending > 0:
		check.Status = ReadinessDegraded
		check.Reason = "async semantic derivations pending"
	case res.Skipped > 0:
		check.Status = ReadinessDegraded
		check.Reason = "async semantic episodes skipped"
	default:
		check.Status = ReadinessReady
	}
	return check
}

func classifyQueueReadiness(backlog, expiredLeases, deadLetters, maxBacklog, maxExpiredLeases, maxDeadLetters int) (ReadinessStatus, string) {
	switch {
	case deadLetters > maxDeadLetters:
		return ReadinessDegraded, "dead letters exceed threshold"
	case expiredLeases > maxExpiredLeases:
		return ReadinessDegraded, "expired leases exceed threshold"
	case backlog > maxBacklog:
		return ReadinessDegraded, "backlog exceeds threshold"
	default:
		return ReadinessReady, ""
	}
}

func (r *ReadinessReport) addCheck(check ReadinessCheck) {
	r.Checks = append(r.Checks, check)
	r.Status = worseReadiness(r.Status, check.Status)
}

func worseReadiness(a, b ReadinessStatus) ReadinessStatus {
	if readinessRank(b) > readinessRank(a) {
		return b
	}
	return a
}

func readinessRank(s ReadinessStatus) int {
	switch s {
	case ReadinessNotReady:
		return 3
	case ReadinessDegraded:
		return 2
	case ReadinessReady:
		return 1
	default:
		return 0
	}
}

var _ ReadinessObserver = (*memory)(nil)
