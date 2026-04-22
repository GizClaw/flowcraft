package ltm

import (
	"sync"
	"time"
)

const (
	defaultCacheMaxEntries    = 10000
	defaultCacheCleanInterval = 60 * time.Second
)

type assemblerCache struct {
	mu         sync.RWMutex
	pinned     map[string]cacheEntry
	recall     map[string]cacheEntry
	lastRecall map[string]recallSnapshot
	maxEntries int
	done       chan struct{}
}

type cacheEntry struct {
	entries []*MemoryEntry
	expires time.Time
}

type recallSnapshot struct {
	query  string
	at     time.Time
	result []*MemoryEntry
}

func newAssemblerCache() *assemblerCache {
	c := &assemblerCache{
		pinned:     make(map[string]cacheEntry),
		recall:     make(map[string]cacheEntry),
		lastRecall: make(map[string]recallSnapshot),
		maxEntries: defaultCacheMaxEntries,
		done:       make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *assemblerCache) close() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func (c *assemblerCache) cleanupLoop() {
	ticker := time.NewTicker(defaultCacheCleanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.evictExpired()
		}
	}
}

func (c *assemblerCache) evictExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, e := range c.pinned {
		if now.After(e.expires) {
			delete(c.pinned, k)
		}
	}
	for k, e := range c.recall {
		if now.After(e.expires) {
			delete(c.recall, k)
		}
	}
}

func (c *assemblerCache) readPinned(key string) ([]*MemoryEntry, bool) {
	c.mu.RLock()
	entry, ok := c.pinned[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.entries, true
}

func (c *assemblerCache) writePinned(key string, entries []*MemoryEntry, ttl time.Duration) {
	cp := make([]*MemoryEntry, len(entries))
	copy(cp, entries)
	c.mu.Lock()
	if len(c.pinned) >= c.maxEntries {
		c.evictOldestLocked(c.pinned)
	}
	c.pinned[key] = cacheEntry{entries: cp, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *assemblerCache) readRecall(key string) ([]*MemoryEntry, bool) {
	c.mu.RLock()
	entry, ok := c.recall[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.entries, true
}

func (c *assemblerCache) writeRecall(key string, entries []*MemoryEntry, ttl time.Duration) {
	cp := make([]*MemoryEntry, len(entries))
	copy(cp, entries)
	c.mu.Lock()
	if len(c.recall) >= c.maxEntries {
		c.evictOldestLocked(c.recall)
	}
	c.recall[key] = cacheEntry{entries: cp, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *assemblerCache) getLastRecall(scopeKey string) (recallSnapshot, bool) {
	c.mu.RLock()
	s, ok := c.lastRecall[scopeKey]
	c.mu.RUnlock()
	return s, ok
}

func (c *assemblerCache) setLastRecall(scopeKey string, query string, result []*MemoryEntry) {
	cp := make([]*MemoryEntry, len(result))
	copy(cp, result)
	c.mu.Lock()
	if len(c.lastRecall) >= c.maxEntries {
		c.evictOldestRecallLocked()
	}
	c.lastRecall[scopeKey] = recallSnapshot{query: query, at: time.Now(), result: cp}
	c.mu.Unlock()
}

// evictOldestLocked removes the entry with the earliest expiration. Caller must hold c.mu.
func (c *assemblerCache) evictOldestLocked(m map[string]cacheEntry) {
	var oldestKey string
	var oldestTime time.Time
	for k, e := range m {
		if oldestKey == "" || e.expires.Before(oldestTime) {
			oldestKey = k
			oldestTime = e.expires
		}
	}
	if oldestKey != "" {
		delete(m, oldestKey)
	}
}

// evictOldestRecallLocked removes the oldest recallSnapshot. Caller must hold c.mu.
func (c *assemblerCache) evictOldestRecallLocked() {
	var oldestKey string
	var oldestTime time.Time
	for k, s := range c.lastRecall {
		if oldestKey == "" || s.at.Before(oldestTime) {
			oldestKey = k
			oldestTime = s.at
		}
	}
	if oldestKey != "" {
		delete(c.lastRecall, oldestKey)
	}
}
