// Package kanban provides an in-memory sdkx/delegation AsyncBackend and
// WorkSource.
//
// Submit and Status expose only backend-neutral delegation ids, requests, and
// responses. Card, Query, Watch, Suspend, Resume, events, and metrics are
// operational APIs for observing and controlling this delegation backend.
package kanban
