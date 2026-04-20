package knowledge

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"
)

// CacheOption configures a CachedStore.
type CacheOption func(*CachedStore)

// WithTTL sets the cache time-to-live.
func WithTTL(d time.Duration) CacheOption {
	return func(s *CachedStore) { s.ttl = d }
}

// WithMaxItems sets the maximum number of cached items.
func WithMaxItems(n int) CacheOption {
	return func(s *CachedStore) { s.maxItems = n }
}

type cacheEntry struct {
	key     string
	value   any
	expiry  time.Time
	element *list.Element
}

// CachedStore wraps a Store with TTL + LRU caching for read operations.
// Write operations are forwarded and evict related cache entries.
type CachedStore struct {
	inner    Store
	mu       sync.RWMutex
	items    map[string]*cacheEntry
	order    *list.List
	ttl      time.Duration
	maxItems int
}

// NewCachedStore wraps inner with caching.
func NewCachedStore(inner Store, opts ...CacheOption) *CachedStore {
	s := &CachedStore{
		inner:    inner,
		items:    make(map[string]*cacheEntry),
		order:    list.New(),
		ttl:      5 * time.Minute,
		maxItems: 1000,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *CachedStore) get(key string) (any, bool) {
	s.mu.RLock()
	entry, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiry) {
		s.mu.Lock()
		s.removeLocked(key)
		s.mu.Unlock()
		return nil, false
	}
	s.mu.Lock()
	s.order.MoveToFront(entry.element)
	s.mu.Unlock()
	return entry.value, true
}

func (s *CachedStore) set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.items[key]; ok {
		entry.value = value
		entry.expiry = time.Now().Add(s.ttl)
		s.order.MoveToFront(entry.element)
		return
	}

	for len(s.items) >= s.maxItems {
		back := s.order.Back()
		if back == nil {
			break
		}
		s.removeLocked(back.Value.(string))
	}

	el := s.order.PushFront(key)
	s.items[key] = &cacheEntry{
		key: key, value: value, expiry: time.Now().Add(s.ttl), element: el,
	}
}

func (s *CachedStore) removeLocked(key string) {
	entry, ok := s.items[key]
	if !ok {
		return
	}
	s.order.Remove(entry.element)
	delete(s.items, key)
}

func (s *CachedStore) evictDataset(datasetID string) {
	prefix := "doc:" + datasetID + "/"
	searchPrefix := "search:" + datasetID + "|"
	absPrefix := "abs:" + datasetID + "/"
	ovPrefix := "ov:" + datasetID + "/"
	dsAbs := "dsabs:" + datasetID
	dsOv := "dsov:" + datasetID

	s.mu.Lock()
	defer s.mu.Unlock()

	for key := range s.items {
		if hasAnyPrefix(key, prefix, searchPrefix, absPrefix, ovPrefix) || key == dsAbs || key == dsOv {
			s.removeLocked(key)
		}
	}
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if len(s) >= len(p) && s[:len(p)] == p {
			return true
		}
	}
	return false
}

// --- Store interface ---

func (s *CachedStore) AddDocument(ctx context.Context, datasetID, name, content string) error {
	if err := s.inner.AddDocument(ctx, datasetID, name, content); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) AddDocuments(ctx context.Context, datasetID string, docs []DocInput) error {
	if err := s.inner.AddDocuments(ctx, datasetID, docs); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) GetDocument(ctx context.Context, datasetID, name string) (*Document, error) {
	key := fmt.Sprintf("doc:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		doc, ok := v.(Document)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.GetDocument(ctx, datasetID, name)
		}
		return &doc, nil
	}
	doc, err := s.inner.GetDocument(ctx, datasetID, name)
	if err != nil {
		return nil, err
	}
	s.set(key, *doc)
	return doc, nil
}

func (s *CachedStore) DeleteDocument(ctx context.Context, datasetID, name string) error {
	if err := s.inner.DeleteDocument(ctx, datasetID, name); err != nil {
		return err
	}
	s.evictDataset(datasetID)
	return nil
}

func (s *CachedStore) ListDocuments(ctx context.Context, datasetID string) ([]Document, error) {
	return s.inner.ListDocuments(ctx, datasetID)
}

func (s *CachedStore) Search(ctx context.Context, datasetID, query string, opts SearchOptions) ([]SearchResult, error) {
	key := fmt.Sprintf("search:%s|%s|%d|%s|%f|%s", datasetID, query, opts.TopK, opts.MaxLayer, opts.Threshold, opts.Mode)
	if v, ok := s.get(key); ok {
		results, ok := v.([]SearchResult)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Search(ctx, datasetID, query, opts)
		}
		cp := make([]SearchResult, len(results))
		copy(cp, results)
		return cp, nil
	}
	results, err := s.inner.Search(ctx, datasetID, query, opts)
	if err != nil {
		return nil, err
	}
	s.set(key, results)
	return results, nil
}

func (s *CachedStore) Abstract(ctx context.Context, datasetID, name string) (string, error) {
	key := fmt.Sprintf("abs:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Abstract(ctx, datasetID, name)
		}
		return val, nil
	}
	val, err := s.inner.Abstract(ctx, datasetID, name)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) Overview(ctx context.Context, datasetID, name string) (string, error) {
	key := fmt.Sprintf("ov:%s/%s", datasetID, name)
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.Overview(ctx, datasetID, name)
		}
		return val, nil
	}
	val, err := s.inner.Overview(ctx, datasetID, name)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) DatasetAbstract(ctx context.Context, datasetID string) (string, error) {
	key := "dsabs:" + datasetID
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.DatasetAbstract(ctx, datasetID)
		}
		return val, nil
	}
	val, err := s.inner.DatasetAbstract(ctx, datasetID)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

func (s *CachedStore) DatasetOverview(ctx context.Context, datasetID string) (string, error) {
	key := "dsov:" + datasetID
	if v, ok := s.get(key); ok {
		val, ok := v.(string)
		if !ok {
			s.mu.Lock()
			s.removeLocked(key)
			s.mu.Unlock()
			return s.inner.DatasetOverview(ctx, datasetID)
		}
		return val, nil
	}
	val, err := s.inner.DatasetOverview(ctx, datasetID)
	if err != nil {
		return "", err
	}
	s.set(key, val)
	return val, nil
}

// EvictDataset removes all cached entries for a dataset. Callers should
// invoke this after mutating the underlying store out-of-band — for
// example, after refreshing layered context on the inner FSStore via
// SetDocAbstract / SetDatasetOverview / etc.
func (s *CachedStore) EvictDataset(datasetID string) {
	s.evictDataset(datasetID)
}
