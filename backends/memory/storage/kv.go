package storage

import "context"

// Entry is one key/value pair returned by a prefix scan.
type Entry struct {
	Key   string
	Value []byte
}

// Store is the key-value substrate for current values and keyed state
// (document current values, checkpoints, watermarks, catalogs, snapshots).
//
// Keys are deterministic, path-like names whose segments are stable for one
// storage version. A key that is a prefix of another key is allowed by the
// contract; backends that cannot represent both (the workspace adapter)
// reject the conflicting write.
type Store interface {
	// Get returns the value at key, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put atomically publishes data at key, overwriting any previous value.
	Put(ctx context.Context, key string, data []byte) error
	// Delete removes the value at key and is idempotent: deleting a
	// missing key returns nil.
	Delete(ctx context.Context, key string) error
	// List returns every entry inside the prefix area (key == prefix or key
	// starts with prefix + "/"), in lexicographic key order. A missing or
	// empty prefix area returns an empty slice, not an error.
	List(ctx context.Context, prefix string) ([]Entry, error)
}

// CASStore is the optional compare-and-swap extension used by lease and
// aggregate workloads.
type CASStore interface {
	// CompareAndSwap publishes new only when the current value equals old.
	// A missing key never matches (old == nil is not a wildcard).
	CompareAndSwap(ctx context.Context, key string, old, new []byte) (bool, error)
}

// BatchStore is the optional batch extension. PutBatch must be atomic: the
// whole batch becomes visible or nothing does. Implementations that cannot
// guarantee that must not expose this interface.
type BatchStore interface {
	PutBatch(ctx context.Context, entries []Entry) error
}

// PutIfAbsentStore is the optional extension for immutable keys (catalog
// entries, repair audits, message commits, projection segments). Immutable
// writes must use PutIfAbsent; the caller decides between idempotent
// success and ErrConflict when it returns false.
type PutIfAbsentStore interface {
	// PutIfAbsent writes data only when key does not exist. It returns
	// false without modifying the key when the key already exists.
	PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error)
}
