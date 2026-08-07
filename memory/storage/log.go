package storage

import (
	"context"
	"encoding/json"
	"time"
)

// Event is one immutable, sequentially numbered entry in a stream.
type Event struct {
	Stream    string          `json:"stream"`
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at,omitempty"`
}

// AppendOptions controls one batch append.
type AppendOptions struct {
	// IdempotencyKey is unique within one stream. Retrying the same key
	// with the same batch returns the original Commit; a different batch
	// under the same key returns ErrConflict.
	IdempotencyKey string
	// Metadata is optional implementation-visible batch metadata.
	Metadata map[string]string
}

// Commit is one immutable, atomically published batch.
type Commit struct {
	ID             string
	Stream         string
	FirstSeq       uint64
	LastSeq        uint64
	IdempotencyKey string
	CreatedAt      time.Time
}

// Log is the append-only substrate for ordered, idempotent event streams
// (conversation messages, publication/observation events, audit trails).
//
// Streams are deterministic, path-like names whose segments are stable for
// one storage version. Batch atomicity is a contract obligation: a single
// Append is either fully visible or not visible at all. Implementations on
// transactional substrates (SQLite/PG) use real transactions; the workspace
// adapter provides single-process atomicity with crash recovery (see
// WorkspaceLog).
type Log interface {
	// Append atomically commits one non-empty batch. Seq values are
	// assigned by the implementation and are strictly increasing within a
	// stream. Retrying the same IdempotencyKey returns the original
	// Commit; a different batch under the same key returns ErrConflict.
	Append(ctx context.Context, stream string, events []Event, opts AppendOptions) (Commit, error)
	// Read returns events in ascending Seq order, strictly after after.
	// A non-positive Limit means no limit. A missing stream returns an
	// empty slice, not an error.
	Read(ctx context.Context, stream string, after uint64, limit int) ([]Event, error)
	// ReadAt returns the single event at seq, or ErrNotFound.
	ReadAt(ctx context.Context, stream string, seq uint64) (Event, error)
	// ReadLatest returns the newest n events in ascending Seq order. A
	// non-positive n means all events.
	ReadLatest(ctx context.Context, stream string, n int) ([]Event, error)
	// ListStreams returns every stream inside the prefix area (name ==
	// prefix or name starts with prefix + "/"), in lexicographic order. A
	// missing or empty prefix area returns an empty slice, not an error.
	ListStreams(ctx context.Context, prefix string) ([]string, error)
}

// CommitLog is the optional Log extension that exposes committed batch
// metadata. It is the authoritative source for reconstructing commit
// listings without a separate index: the workspace adapter reads its commit
// markers, and transactional backends can expose the same view from their
// batch tables.
type CommitLog interface {
	// ListCommits returns committed batches in ascending FirstSeq order,
	// strictly after after. A non-positive Limit means no limit. A missing
	// stream returns an empty slice, not an error.
	ListCommits(ctx context.Context, stream string, after uint64, limit int) ([]Commit, error)
	// ReadCommitByKey returns the committed batch for one idempotency key,
	// or false when the key was never committed.
	ReadCommitByKey(ctx context.Context, stream, idempotencyKey string) (Commit, bool, error)
}
