package scheduler

import "context"

const (
	// MetaScheduleID identifies the schedule that created a business task.
	MetaScheduleID = "schedule_id"
	// MetaDeliveryID is the stable delivery idempotency key.
	MetaDeliveryID = "delivery_id"
	// MetaExecutionID identifies the scheduler execution.
	MetaExecutionID = "execution_id"
)

// Control manages recurring and one-shot schedules.
//
// PutRule is create-or-replace by caller-provided namespace and ID: a different
// rule at the same key replaces the prior rule and is not a conflict.
// ScheduleOnce uses that key for idempotency: an identical replay is safe, but
// a different one-shot at the same key should be classified with
// errdefs.Conflict while its idempotency record is retained.
type Control interface {
	PutRule(context.Context, Rule) error
	DeleteRule(ctx context.Context, namespace, id string) error
	ListRules(ctx context.Context, namespace string) ([]Rule, error)
	ScheduleOnce(context.Context, Once) error
	CancelOnce(ctx context.Context, namespace, id string) error
}

// WorkSource exposes leased executions to remote-capable workers.
//
// Claim returns (nil, nil) when no work is currently available. Renew and
// Complete identify lease ownership solely by execution ID and lease token.
// Implementations should classify stale leases with errdefs.Conflict.
//
// Complete must be idempotent for identical requests through RetainUntil when
// supplied, subject to a documented server maximum. A prior attempt may have
// committed successfully even when its response was lost.
type WorkSource interface {
	Claim(context.Context, ClaimRequest) (*Delivery, error)
	Renew(context.Context, RenewRequest) error
	Complete(context.Context, CompleteRequest) error
}

// Server is the complete scheduler protocol.
type Server interface {
	Control
	WorkSource
}
