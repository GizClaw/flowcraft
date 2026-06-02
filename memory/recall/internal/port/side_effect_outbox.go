package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// SideEffectJobKind names commit-after work enqueued during the
// canonical write lock. Workers drain these jobs outside the lock.
type SideEffectJobKind string

const (
	SideEffectProjectRequired        SideEffectJobKind = "project_required"
	SideEffectProjectOptional        SideEffectJobKind = "project_optional"
	SideEffectProjectEpisodeEvidence SideEffectJobKind = "project_episode_evidence"
	SideEffectEmbeddingBackfill      SideEffectJobKind = "embedding_backfill"
	SideEffectEvolutionAfterSave     SideEffectJobKind = "evolution_after_save"
)

// SideEffectJob is one durable commit-after unit. RequestID groups jobs
// from a single Save so Cancel can drop the whole batch on rollback.
type SideEffectJob struct {
	ID        string
	RequestID string
	Scope     domain.Scope
	Kind      SideEffectJobKind

	// ScopeGeneration is the partition generation captured under the write
	// lock when the job was enqueued. Workers must skip jobs whose generation no
	// longer matches the owning partition after ForgetAll(Hard) / ExpireRetired.
	ScopeGeneration uint64

	Facts []domain.TemporalFact

	Attempt    int
	LeaseUntil time.Time
	LeaseToken string
}

// SideEffectClaimOptions controls commit-after worker leasing.
// Scope is required by the public processor and matches a partition key.
type SideEffectClaimOptions struct {
	WorkerID string
	Scope    domain.Scope
	Now      time.Time
	Max      int
}

// SideEffectResult records successful job completion metadata.
type SideEffectResult struct {
	CompletedAt time.Time
}

// SideEffectFailure records failed job metadata. Transient failures
// return to pending after RetryAt; permanent failures stay terminal.
type SideEffectFailure struct {
	ErrClass diagnostic.ErrClass
	Err      string
	RetryAt  time.Time
}

// SideEffectStats is the operator-facing side-effect outbox snapshot.
type SideEffectStats struct {
	Pending        int
	Leased         int
	ExpiredLeases  int
	Failed         int
	DeadLetter     int
	Completed      int
	CancelledTotal int
}

// SideEffectOutbox is the unified outbox for projection / evolution /
// embedding side effects. Enqueue runs under the scope write lock and
// is idempotent by job ID (default RequestID|Kind). Processors Claim
// leased jobs and Complete/Fail them outside the lock. Terminal jobs
// must scrub raw fact payloads by default; Stats is partition-scoped.
type SideEffectOutbox interface {
	Enqueue(ctx context.Context, job SideEffectJob) error
	Claim(ctx context.Context, opts SideEffectClaimOptions) ([]SideEffectJob, error)
	Complete(ctx context.Context, jobID, leaseToken string, result SideEffectResult) error
	Fail(ctx context.Context, jobID, leaseToken string, failure SideEffectFailure) error
	Cancel(ctx context.Context, requestID string) error
	CancelScope(ctx context.Context, scope domain.Scope) (int, error)
	PurgeScope(ctx context.Context, scope domain.Scope) (int, error)
	Stats(ctx context.Context, scope domain.Scope, now time.Time) (SideEffectStats, error)
}

// SideEffectExecutor runs one drained job. Required projection failures
// must return a non-nil error so processors can retry or dead-letter.
type SideEffectExecutor interface {
	Run(ctx context.Context, job SideEffectJob) error
}
