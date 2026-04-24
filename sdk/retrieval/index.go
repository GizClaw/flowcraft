package retrieval

import (
	"context"
	"io"
)

// Index is the minimal retrieval backend.
type Index interface {
	Upsert(ctx context.Context, namespace string, docs []Doc) error
	Delete(ctx context.Context, namespace string, ids []string) error
	Search(ctx context.Context, namespace string, req SearchRequest) (*SearchResponse, error)
	List(ctx context.Context, namespace string, req ListRequest) (*ListResponse, error)
	Capabilities() Capabilities
	Close() error
}

// DocGetter is implemented by indexes that can read a single document by ID.
type DocGetter interface {
	Get(ctx context.Context, namespace, id string) (Doc, bool, error)
}

// Filterable marks backends that can push down filters natively.
type Filterable interface {
	SupportsFilter(f Filter) bool
}

// Hybridable marks backends with native hybrid search.
type Hybridable interface {
	SearchHybrid(ctx context.Context, namespace string, req HybridRequest) (*SearchResponse, error)
}

// HybridRequest is used by Hybridable backends.
type HybridRequest struct {
	QueryText   string
	QueryVector []float32
	Filter      Filter
	TopK        int
	Mode        HybridMode
	Param       map[string]any

	// Debug controls how much execution detail Hybridable backends should
	// attach to SearchResponse.Execution. Zero value disables it.
	Debug SearchDebug
}

// Vectorizable marks backends that embed internally.
type Vectorizable interface {
	UpsertWithEmbed(ctx context.Context, namespace string, docs []Doc) error
	SearchByText(ctx context.Context, namespace string, text string, topK int) (*SearchResponse, error)
}

// Snapshottable marks backends that support backup/restore.
type Snapshottable interface {
	Snapshot(ctx context.Context, namespace string, dst io.Writer) error
	Restore(ctx context.Context, namespace string, src io.Reader) error
}

// Iterable supports full scans for reindex/migration.
type Iterable interface {
	Iterate(ctx context.Context, namespace string, cursor string, batch int) ([]Doc, string, error)
}

// DeletableByFilter supports bulk delete by metadata predicate.
type DeletableByFilter interface {
	DeleteByFilter(ctx context.Context, namespace string, f Filter) (deleted int64, err error)
}

// Droppable supports O(1) namespace removal (e.g., DROP TABLE, file delete).
//
// Backends without native support should not implement this interface; callers
// fall back to DeleteByFilter or List+Delete ( PostgresIndex).
type Droppable interface {
	Drop(ctx context.Context, namespace string) error
}
