// Package objstore provides a workspace.Workspace implementation backed
// by object storage services (S3, OSS, MinIO, GCS, R2, etc.).
//
// The design separates two concerns:
//
//   - ObjectStore: flat key-value operations against a bucket/container.
//     Each cloud provider implements this interface once.
//
//   - Workspace adapter: translates the tree-shaped workspace.Workspace
//     API into flat ObjectStore calls, handling directory simulation,
//     path validation, and append strategy.
package objstore

import (
	"context"
	"time"
)

// ObjectStore is the minimal set of operations any object-storage backend
// must support. Keys are slash-separated, no leading slash.
//
// Implementations must be safe for concurrent use.
type ObjectStore interface {
	// Get retrieves the object content. Returns ErrKeyNotFound if absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// Put creates or overwrites an object.
	Put(ctx context.Context, key string, data []byte) error

	// Del removes a single object. No error if already absent.
	Del(ctx context.Context, key string) error

	// Head returns metadata without fetching the body.
	// Returns ErrKeyNotFound if absent.
	Head(ctx context.Context, key string) (ObjectInfo, error)

	// ListPrefix returns objects and common prefixes under the given prefix.
	// When delimiter is non-empty (typically "/"), it enables hierarchical
	// listing: keys after the first delimiter occurrence are grouped into
	// CommonPrefixes rather than returned individually.
	ListPrefix(ctx context.Context, prefix, delimiter string) (*ListResult, error)

	// DelPrefix removes all objects whose key starts with prefix.
	DelPrefix(ctx context.Context, prefix string) error
}

// Appender is an optional interface. If the underlying store supports
// native append (e.g. Alibaba OSS AppendObject), it should implement this.
// The workspace adapter falls back to Get+Put when unavailable.
type Appender interface {
	Append(ctx context.Context, key string, data []byte) error
}

// ObjectInfo holds metadata for a single stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
	ContentType  string
	ETag         string
}

// ListResult is the response from a prefix listing.
type ListResult struct {
	// Objects are keys that directly match the prefix (considering delimiter).
	Objects []ObjectInfo

	// CommonPrefixes are "directory" groupings when a delimiter is used.
	// For example, listing prefix="logs/" with delimiter="/" might return
	// CommonPrefixes=["logs/2024/", "logs/2025/"].
	CommonPrefixes []string
}
