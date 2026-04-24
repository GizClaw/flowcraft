package knowledge

import "context"

// EventKind classifies a ChangeEvent emitted by an EventNotifier
// implementation. Used by EventReloader to decide whether to perform a
// targeted or dataset-wide rebuild.
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
// NOTE (v0.2.x): The deprecated ChangeNotifier in deprecated.go still
// emits opaque struct{} events; sdkx/knowledge/watcher remains its only
// in-tree producer. The ChangeEvent shape declared here is what
// EventNotifier implementations will emit once watcher migrates in
// v0.3.0.
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
// Service satisfies this interface; EventReloader invokes Rebuild on the
// trailing edge of a debounce window over an EventNotifier stream.
type Rebuilder interface {
	Rebuild(ctx context.Context, scope RebuildScope) error
}
