// Package kanban provides an in-memory core/delegation AsyncBackend and
// WorkSource.
//
// Submit and Status expose only backend-neutral delegation ids, requests, and
// responses. Card, Query, Watch, Suspend, Resume, events, and metrics are
// operational APIs for observing and controlling this delegation backend.
// Terminal cards may leave those operational views before the configured
// idempotency retention ends; a minimal tombstone keeps keyed submissions
// replayable and terminal responses queryable until that finite window expires.
package kanban
