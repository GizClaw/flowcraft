package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// AsyncSemanticQueue is a durable outbox for semantic extraction work
// items produced by Memory.Save(WriteModeAsyncSemantic).
//
// Implementations MUST:
//   - Make Enqueue idempotent on AsyncSemanticJob.RequestID.
//   - Treat Enqueue as a local-durable outbox boundary — Save holds the
//     scope write lock across Enqueue, so backends MUST complete in
//     under ~10ms p99. Remote queue backends MUST drain to the remote
//     service in a backend-internal worker, outside the scope lock.
//   - Preserve FIFO claim order within a scope (sorted by enqueued_at,
//     then request_id) so resolver decisions across jobs stay
//     consistent with sync writes.
type AsyncSemanticQueue interface {
	Enqueue(ctx context.Context, job AsyncSemanticJob) (AsyncSemanticReceipt, error)
	// Cancel removes a previously enqueued job by RequestID. It backs
	// the write_semantic_outbox Compensator when a downstream stage
	// (structured facts) fails after a successful Enqueue. Idempotent
	// on unknown or already-completed request IDs.
	Cancel(ctx context.Context, requestID string) error
	// CancelScope removes every non-complete job for the supplied
	// scope partition (PartitionKey). It backs ForgetAll(Hard) so
	// workers do not derive semantic facts after a full scope wipe.
	CancelScope(ctx context.Context, scope domain.Scope) (int, error)
	// PurgeScope removes every job in the partition, including
	// completed and dead-letter entries, and clears enqueue-time PII
	// snapshots (TurnsSnapshot, RecentMessages, ExistingFactsAnchor).
	// It backs ForgetAll(Hard) after CancelScope so durable outbox
	// rows cannot leak post-wipe.
	PurgeScope(ctx context.Context, scope domain.Scope) (int, error)
	// CancelMatchingEpisodes removes non-complete jobs in scope whose
	// EpisodeFactIDs intersect deletedEpisodeFactIDs. Idempotent when
	// the slice is empty. Used by ExpireRetired and ForgetAll(Soft).
	CancelMatchingEpisodes(ctx context.Context, scope domain.Scope, deletedEpisodeFactIDs []string) (int, error)
	Claim(ctx context.Context, opts AsyncSemanticClaimOptions) ([]AsyncSemanticJob, error)
	Complete(ctx context.Context, requestID, leaseToken string, result AsyncSemanticResult) error
	Fail(ctx context.Context, requestID, leaseToken string, failure AsyncSemanticFailure) error
	// Stats returns queue depth and terminal-state counts for operators.
	// Implementations MUST require a non-zero Scope partition (RuntimeID
	// and UserID); global cross-tenant stats are not supported.
	Stats(ctx context.Context, filter AsyncSemanticStatsFilter) (AsyncSemanticStats, error)
}

// AsyncSemanticRequeueQueue is an optional reconcile extension for queues that
// can reset a terminal/missing request back to pending from canonical episodes.
type AsyncSemanticRequeueQueue interface {
	Requeue(ctx context.Context, job AsyncSemanticJob) (AsyncSemanticReceipt, bool, error)
}

// AsyncSemanticStatsFilter scopes Stats to one scope partition.
type AsyncSemanticStatsFilter struct {
	Scope domain.Scope
	Now   time.Time
}

// AsyncSemanticStats is the operator-facing queue health snapshot.
// DeadLetter counts failed jobs whose ErrClass is permanent;
// ExpiredLeases counts leased jobs past LeaseUntil at Now.
type AsyncSemanticStats struct {
	Pending        int
	Leased         int
	ExpiredLeases  int
	Failed         int
	DeadLetter     int
	Completed      int
	CancelledTotal int
}

// AsyncSemanticClaimOptions controls Claim batching and tenancy
// filters. Zero Max defaults to no jobs; callers should set Max
// explicitly. ProcessAsyncSemantic requires Scope so ordinary workers
// claim exactly one partition; RuntimeID exists only for lower-level
// queue implementations or future privileged/admin drains. When both
// Scope and RuntimeID are set, Scope wins (exact partition match).
type AsyncSemanticClaimOptions struct {
	WorkerID  string
	Now       time.Time
	Max       int
	Scope     *domain.Scope
	RuntimeID string
}

// AsyncSemanticJob is one durable semantic extraction work item.
type AsyncSemanticJob struct {
	RequestID string
	Scope     domain.Scope

	EpisodeFactIDs []string
	// TurnsSnapshot is an optional compressed fallback for backends
	// that cannot cheaply read the canonical store during processing.
	// Workers SHOULD prefer reconstructing turns from EpisodeFactIDs.
	TurnsSnapshot []domain.TurnContext

	ObservedAt time.Time
	Tier       string

	// RecentMessages / ExistingFactsAnchor are enqueue-time snapshots,
	// NOT live reads. Delayed workers must extract against the context
	// that existed when the user-facing Save was accepted.
	RecentMessages      []domain.Message
	ExistingFactsAnchor []domain.TemporalFact

	Attempt    int
	LeaseUntil time.Time
	// LeaseToken is assigned by Claim; Complete / Fail must supply
	// the same token so an expired worker cannot ack a re-claimed job.
	LeaseToken string
}

// AsyncSemanticReceipt is the return value of Enqueue and the wire
// contract observable to Memory.Save callers via SaveResult.
type AsyncSemanticReceipt struct {
	RequestID  string
	EnqueuedAt time.Time
	QueueDepth int
}

// AsyncSemanticResult is the worker's success report. Trace is owned
// by the processor and intentionally absent here so the async save
// facade does not depend on the processor surface.
type AsyncSemanticResult struct {
	SemanticFactIDs []string
	// RecoveredFromPriorAttempt is true when the worker discovered the
	// semantic facts already existed via origin index lookup (a prior
	// attempt crashed between append and complete). Trace will be empty.
	RecoveredFromPriorAttempt bool
}

// AsyncSemanticFailure is the worker's failure report. ErrClass drives
// retry vs dead-letter routing in the queue backend.
type AsyncSemanticFailure struct {
	ErrClass diagnostic.ErrClass
	Err      string
	RetryAt  time.Time
}
