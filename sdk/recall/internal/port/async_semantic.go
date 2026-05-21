package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
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
	Claim(ctx context.Context, workerID string, now time.Time, max int) ([]AsyncSemanticJob, error)
	Complete(ctx context.Context, requestID string, result AsyncSemanticResult) error
	Fail(ctx context.Context, requestID string, failure AsyncSemanticFailure) error
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
}

// AsyncSemanticReceipt is the return value of Enqueue and the wire
// contract observable to Memory.Save callers via SaveResult.
type AsyncSemanticReceipt struct {
	RequestID  string
	EnqueuedAt time.Time
	QueueDepth int
}

// AsyncSemanticResult is the worker's success report. Trace is owned
// by the processor in F.1b and intentionally absent here so F.1a does
// not depend on the processor surface.
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
