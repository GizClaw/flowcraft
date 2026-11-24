// Package cache holds bounded, concurrency-safe caches for values that are
// expensive to derive and cheap to keep.
//
// It exists because several adapters decode the same immutable input on every
// request — a tool's JSON Schema, a response format's schema — and a decoded copy is
// pure: the same bytes can only produce the same value. A cache turns that repeated
// work into one map lookup, and the least-recently-used order is what keeps it useful
// in a long-running process whose working set rotates instead of growing.
//
// These caches are per instance, not global: a process can serve several deployments,
// and none of them should be able to evict another's entries. The package is internal
// because only the providers in this module need it today; a driver in the separate
// driver module cannot import it, so promoting it to a public home is a deliberate
// step to take when one actually does.
//
// A cached value is shared by every caller that asks for it, so it must be treated as
// immutable: a caller that mutates what it got back changes the answer for everyone
// else holding the same entry. Cache values are decoded payloads the provider SDK
// only reads while marshalling, which is what makes sharing them safe.
package cache

import (
	"container/list"
	"sync"
)

// LRU is a bounded least-recently-used cache. Capacity is enforced by eviction, so
// storing a new key when full drops the entry that has gone longest without a read;
// the zero value is not usable, use [New].
//
// It is safe for concurrent use. The lock covers one map operation, so readers and
// writers contend only for that, not for the caller's work.
type LRU[V any] struct {
	mu       sync.Mutex
	capacity int
	order    *list.List
	items    map[string]*list.Element
}

// entry is one cached value plus the key its element is filed under, so eviction can
// remove the map entry without a second lookup.
type entry[V any] struct {
	key   string
	value V
}

// New returns a cache holding at most capacity entries. A capacity below one is
// raised to one: a caller that passes zero is misconfigured, and a cache that cannot
// hold anything would still have to answer, so it answers with the smallest bound
// rather than panicking.
func New[V any](capacity int) *LRU[V] {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU[V]{
		capacity: capacity,
		order:    list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// Get returns the value stored under key and marks it the most recently used. A miss
// reports false and leaves the cache unchanged.
func (c *LRU[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*entry[V]).value, true
}

// Put stores value under key, replacing an existing entry in place and evicting the
// least recently used entry when the cache is full.
func (c *LRU[V]) Put(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		stored := element.Value.(*entry[V])
		stored.value = value
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(&entry[V]{key: key, value: value})
	c.items[key] = element
	if c.order.Len() > c.capacity {
		c.evict()
	}
}

// evict drops the least recently used entry. It runs with the lock held.
func (c *LRU[V]) evict() {
	oldest := c.order.Back()
	if oldest == nil {
		return
	}
	c.order.Remove(oldest)
	delete(c.items, oldest.Value.(*entry[V]).key)
}

// Len reports how many entries are cached. It is a diagnostic: a caller should not
// branch on it.
func (c *LRU[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Capacity reports the bound the cache was built with.
func (c *LRU[V]) Capacity() int { return c.capacity }

// Clear drops every entry. It exists for tests and for a host that wants to release
// memory without discarding the provider.
func (c *LRU[V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.items = make(map[string]*list.Element, c.capacity)
}
