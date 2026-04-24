package knowledge

import "context"

// EventKind classifies a ChangeEvent emitted by a future ChangeNotifier
// implementation. Used by the v0.3.0 Reloader to decide whether to perform
// a targeted or dataset-wide rebuild.
type EventKind int

const (
	// EventPut signals that a single document was created or updated.
	EventPut EventKind = iota
	// EventDelete signals that a single document was removed.
	EventDelete
	// EventBulk signals a dataset-level mass change (e.g. snapshot replaced).
	EventBulk
)

// ChangeEvent carries enough granularity for targeted rebuilds.
// DocName == "" denotes a dataset-level event.
//
// NOTE (v0.2.x): The legacy ChangeNotifier in reloader.go still emits
// struct{} events. The new ChangeEvent shape will be wired up in v0.3.0
// when the legacy Reloader is replaced.
type ChangeEvent struct {
	DatasetID string
	DocName   string
	Kind      EventKind
}

// RebuildScope narrows what Rebuilder.Rebuild touches. Zero value means
// "everything".
type RebuildScope struct {
	DatasetID string // "" means all datasets
	DocName   string // "" means all documents in the dataset
}

// Rebuilder is the consumer side of the change-driven reload pipeline.
// Service satisfies this interface; future ChangeNotifier implementations
// invoke Rebuild on the trailing edge of a debounce window.
type Rebuilder interface {
	Rebuild(ctx context.Context, scope RebuildScope) error
}
