// Package syncx provides synchronization primitives that round out
// the stdlib `sync` toolkit for sdk-internal use.
//
// This package is INTERNAL by design — it deliberately sits under
// sdk/internal/ so external modules cannot take a dependency on
// primitives that we may evolve aggressively. Sdkx and downstream
// modules duplicate / port primitives as needed.
package syncx

import "sync"

// KeyedMutex is a map of locks keyed by an arbitrary string. Use it
// to serialize work that targets the SAME logical key while
// allowing UNRELATED keys to proceed in parallel — the canonical
// motivation is per-conversation Append (sdk/history) and per-
// namespace history-store fallback (sdk/recall), where two
// goroutines touching different keys must not contend.
//
// Compared to a plain map[string]*sync.Mutex guarded by an outer
// mutex, KeyedMutex tracks reference counts so the entry for a key
// is released back to the pool when no goroutine holds or waits
// for it. That keeps memory bounded by max concurrent active keys,
// not lifetime distinct keys — important when keys are
// per-conversation ULIDs that churn over time.
//
// Zero-value [KeyedMutex] is ready to use; the [New] constructor
// is a documentation alias and not strictly required.
//
// Concurrency: KeyedMutex is itself safe for concurrent use by any
// number of goroutines. Lock blocks the caller until it holds the
// per-key lock; Unlock releases it (and reaps the entry when no
// one else cares).
type KeyedMutex struct {
	mu      sync.Mutex
	entries map[string]*kmEntry
}

type kmEntry struct {
	mu  sync.Mutex
	cnt int // number of goroutines that have Lock'd or are waiting; entry is reaped when cnt drops to 0
}

// New returns a fresh KeyedMutex. The zero value works just as
// well — New exists only as a documentation alias for call sites
// that prefer the constructor idiom.
func New() *KeyedMutex { return &KeyedMutex{} }

// Lock blocks until the caller holds the per-key lock for key.
// Two Lock(K) calls with the same key serialise; Lock(K1) and
// Lock(K2) for distinct keys never contend.
//
// Pair every Lock with exactly one Unlock for the same key —
// usually via `defer km.Unlock(key)` immediately after Lock
// returns. Unlock with no matching Lock panics; Lock recursively
// on the same key from the same goroutine deadlocks (KeyedMutex
// is not re-entrant, matching [sync.Mutex] semantics).
func (k *KeyedMutex) Lock(key string) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = make(map[string]*kmEntry)
	}
	e, ok := k.entries[key]
	if !ok {
		e = &kmEntry{}
		k.entries[key] = e
	}
	e.cnt++
	k.mu.Unlock()

	e.mu.Lock()
}

// Unlock releases the per-key lock for key. Calling Unlock without
// a matching prior Lock panics (mirrors sync.Mutex). Safe to call
// Unlock(K1) and Unlock(K2) for distinct keys concurrently.
func (k *KeyedMutex) Unlock(key string) {
	k.mu.Lock()
	e, ok := k.entries[key]
	if !ok {
		k.mu.Unlock()
		panic("syncx: KeyedMutex.Unlock on key with no matching Lock: " + key)
	}
	e.cnt--
	if e.cnt == 0 {
		delete(k.entries, key)
	}
	k.mu.Unlock()
	// Unlock the per-key mutex AFTER releasing the map lock so a
	// waiter that wakes up in Lock above does not race the
	// post-Unlock map state. The waiter's e.mu.Lock returns and it
	// proceeds — the entry it holds is still valid (e is heap-
	// allocated; the map delete only drops the map's reference).
	e.mu.Unlock()
}

// WithLock is a convenience wrapper around Lock + defer Unlock. It
// is the recommended call-site pattern for callers that want a
// scoped critical section without managing the defer themselves.
func (k *KeyedMutex) WithLock(key string, fn func()) {
	k.Lock(key)
	defer k.Unlock(key)
	fn()
}
