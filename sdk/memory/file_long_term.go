package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/textsearch"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/rs/xid"

	otellog "go.opentelemetry.io/otel/log"
)

// FileLongTermStore persists long-term memory as JSONL files, organized by
// memory/long_term/{runtimeID}/{category}.jsonl. Uses textsearch.CorpusStats
// for BM25 scoring. Thread-safe via sync.RWMutex.
type FileLongTermStore struct {
	ws         workspace.Workspace
	prefix     string
	maxEntries int

	// per-user per-category cache and corpus stats (lazy-loaded)
	cache   map[string]map[MemoryCategory][]*MemoryEntry
	corpora map[string]map[MemoryCategory]*textsearch.CorpusStats

	mu sync.RWMutex
}

// LTStoreOption configures a FileLongTermStore.
type LTStoreOption func(*FileLongTermStore)

// WithMaxEntries sets the maximum number of entries per category.
// 0 means no limit.
func WithMaxEntries(n int) LTStoreOption {
	return func(s *FileLongTermStore) { s.maxEntries = n }
}

// NewFileLongTermStore creates a file-based long-term memory store.
func NewFileLongTermStore(ws workspace.Workspace, prefix string, opts ...LTStoreOption) *FileLongTermStore {
	if prefix == "" {
		prefix = "memory/long_term"
	}
	s := &FileLongTermStore{
		ws:      ws,
		prefix:  prefix,
		cache:   make(map[string]map[MemoryCategory][]*MemoryEntry),
		corpora: make(map[string]map[MemoryCategory]*textsearch.CorpusStats),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *FileLongTermStore) categoryPath(runtimeID string, category MemoryCategory) string {
	return fmt.Sprintf("%s/%s/%s.jsonl", s.prefix, runtimeID, category)
}

func (s *FileLongTermStore) Save(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.ID == "" {
		entry.ID = xid.New().String()
	}
	now := time.Now()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now

	// Warm cache from disk BEFORE appending, so the new entry isn't double-counted
	s.ensureCacheWarmed(ctx, runtimeID, entry.Category)

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("long_term: marshal entry: %w", err)
	}
	data = append(data, '\n')

	path := s.categoryPath(runtimeID, entry.Category)
	if err := s.ws.Append(ctx, path, data); err != nil {
		return fmt.Errorf("long_term: append %q: %w", path, err)
	}

	s.appendToCache(runtimeID, entry.Category, entry)
	s.getCorpus(runtimeID, entry.Category).AddDocument(s.tokenizeEntry(entry))

	s.evictIfNeeded(ctx, runtimeID, entry.Category)
	return nil
}

func (s *FileLongTermStore) List(ctx context.Context, runtimeID string, opts ListOptions) ([]*MemoryEntry, error) {
	categories := []MemoryCategory{opts.Category}

	s.mu.Lock()
	if opts.Category == "" {
		categories = s.cachedCategories(runtimeID)
	}
	snapshots := make(map[MemoryCategory][]*MemoryEntry, len(categories))
	for _, cat := range categories {
		entries, err := s.loadCategoryLocked(ctx, runtimeID, cat)
		if err != nil {
			continue
		}
		snapshots[cat] = entries
	}
	s.mu.Unlock()

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	var all []*MemoryEntry
	rec := EffectiveRecallForList(opts, runtimeID)
	for _, entries := range snapshots {
		for _, e := range entries {
			if rec != nil {
				if !EntryMatchesRecallScope(e, rec) {
					continue
				}
			} else if !EntryMatchesQueryScope(e, opts.Scope) {
				continue
			}
			all = append(all, e)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].UpdatedAt.After(all[j].UpdatedAt)
	})

	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (s *FileLongTermStore) Search(ctx context.Context, runtimeID string, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	tokenizer := textsearch.DetectTokenizer(query)
	keywords := textsearch.ExtractKeywords(query, tokenizer)
	if len(keywords) == 0 {
		return nil, nil
	}

	topK := opts.TopK
	if topK <= 0 {
		topK = 10
	}

	categories := []MemoryCategory{opts.Category}

	// Take snapshot under lock, then score outside lock.
	s.mu.Lock()
	if opts.Category == "" {
		categories = s.cachedCategories(runtimeID)
	}
	type catSnapshot struct {
		entries []*MemoryEntry
		corpus  *textsearch.CorpusStats
	}
	snapshots := make([]catSnapshot, 0, len(categories))
	for _, cat := range categories {
		entries, err := s.loadCategoryLocked(ctx, runtimeID, cat)
		if err != nil || len(entries) == 0 {
			continue
		}
		snapshots = append(snapshots, catSnapshot{entries: entries, corpus: s.getCorpus(runtimeID, cat)})
	}
	s.mu.Unlock()

	type scored struct {
		entry *MemoryEntry
		score float64
	}
	var results []scored

	now := time.Now()
	rec := EffectiveRecallForSearch(opts, runtimeID)
	for _, snap := range snapshots {
		for _, e := range snap.entries {
			if rec != nil {
				if !EntryMatchesRecallScope(e, rec) {
					continue
				}
			} else if !EntryMatchesQueryScope(e, opts.Scope) {
				continue
			}
			docText := e.Content + " " + strings.Join(e.Keywords, " ")
			score := textsearch.ScoreText(docText, keywords, snap.corpus, tokenizer)
			if score > 0 {
				score *= TimeDecay(e.UpdatedAt, e.Category, now)
			}
			if score >= opts.Threshold {
				results = append(results, scored{e, score})
			}
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]*MemoryEntry, len(results))
	for i, r := range results {
		out[i] = r.entry
	}
	return out, nil
}

func (s *FileLongTermStore) Update(ctx context.Context, runtimeID string, entry *MemoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cats := s.cachedCategories(runtimeID)
	if entry.Category != "" {
		cats = []MemoryCategory{entry.Category}
	}

	for _, cat := range cats {
		entries, err := s.loadCategoryLocked(ctx, runtimeID, cat)
		if err != nil {
			continue
		}
		for i, e := range entries {
			if e.ID == entry.ID {
				kw := entry.Keywords
				if len(kw) == 0 {
					kw = e.Keywords
				}
				src := entry.Source
				if src == (MemorySource{}) {
					src = e.Source
				}
				merged := &MemoryEntry{
					ID:        entry.ID,
					Category:  cat,
					Content:   entry.Content,
					Keywords:  kw,
					Source:    src,
					CreatedAt: e.CreatedAt,
					UpdatedAt: time.Now(),
				}
				entries[i] = merged
				return s.writeCategory(ctx, runtimeID, cat, entries)
			}
		}
	}
	return fmt.Errorf("long_term: entry %q not found", entry.ID)
}

func (s *FileLongTermStore) Delete(ctx context.Context, runtimeID, entryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cat := range s.cachedCategories(runtimeID) {
		entries, err := s.loadCategoryLocked(ctx, runtimeID, cat)
		if err != nil {
			continue
		}
		for i, e := range entries {
			if e.ID == entryID {
				updated := make([]*MemoryEntry, 0, len(entries)-1)
				updated = append(updated, entries[:i]...)
				updated = append(updated, entries[i+1:]...)
				return s.writeCategory(ctx, runtimeID, cat, updated)
			}
		}
	}
	return nil
}

// loadCategoryLocked returns entries from cache if available, otherwise loads
// from disk and populates both cache and corpus. Caller must hold s.mu.Lock.
func (s *FileLongTermStore) loadCategoryLocked(ctx context.Context, runtimeID string, category MemoryCategory) ([]*MemoryEntry, error) {
	if userCache, ok := s.cache[runtimeID]; ok {
		if entries, ok := userCache[category]; ok {
			return entries, nil
		}
	}
	entries, err := s.readCategoryFromDisk(ctx, runtimeID, category)
	if err != nil {
		return nil, err
	}
	s.setCache(runtimeID, category, entries)
	s.rebuildCorpus(runtimeID, category, entries)
	return entries, nil
}

func (s *FileLongTermStore) readCategoryFromDisk(ctx context.Context, runtimeID string, category MemoryCategory) ([]*MemoryEntry, error) {
	path := s.categoryPath(runtimeID, category)
	exists, err := s.ws.Exists(ctx, path)
	if err != nil || !exists {
		return nil, err
	}

	data, err := s.ws.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("long_term: read %q: %w", path, err)
	}

	var entries []*MemoryEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry MemoryEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entries = append(entries, &entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("long_term: scan %q: %w", path, err)
	}
	return entries, nil
}

func (s *FileLongTermStore) writeCategory(ctx context.Context, runtimeID string, category MemoryCategory, entries []*MemoryEntry) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("long_term: encode entry: %w", err)
		}
	}

	path := s.categoryPath(runtimeID, category)
	if err := s.ws.Write(ctx, path, buf.Bytes()); err != nil {
		return err
	}

	s.setCache(runtimeID, category, entries)
	s.rebuildCorpus(runtimeID, category, entries)
	return nil
}

func (s *FileLongTermStore) evictIfNeeded(ctx context.Context, runtimeID string, category MemoryCategory) {
	if s.maxEntries <= 0 {
		return
	}
	entries, err := s.loadCategoryLocked(ctx, runtimeID, category)
	if err != nil || len(entries) <= s.maxEntries {
		return
	}
	sorted := make([]*MemoryEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].UpdatedAt.After(sorted[j].UpdatedAt)
	})
	if err := s.writeCategory(ctx, runtimeID, category, sorted[:s.maxEntries]); err != nil {
		telemetry.Warn(ctx, "long_term: eviction write failed",
			otellog.String("runtime_id", runtimeID),
			otellog.String("category", string(category)),
			otellog.String("error", err.Error()))
	}
}

// --- cache helpers ---

// cachedCategories returns categories that exist in the cache for a given
// runtimeID. Falls back to AllCategories() if cache is empty (cold start).
// Caller must hold s.mu.
func (s *FileLongTermStore) cachedCategories(runtimeID string) []MemoryCategory {
	userCache, ok := s.cache[runtimeID]
	if !ok || len(userCache) == 0 {
		return AllCategories()
	}
	cats := make([]MemoryCategory, 0, len(userCache))
	for cat := range userCache {
		cats = append(cats, cat)
	}
	return cats
}

// ensureCacheWarmed loads disk data into cache if cold. Must be called
// under s.mu.Lock, before any append to disk for the same category.
func (s *FileLongTermStore) ensureCacheWarmed(ctx context.Context, runtimeID string, category MemoryCategory) {
	if _, ok := s.cache[runtimeID]; !ok {
		s.cache[runtimeID] = make(map[MemoryCategory][]*MemoryEntry)
	}
	if _, ok := s.cache[runtimeID][category]; ok {
		return
	}
	if existing, err := s.readCategoryFromDisk(ctx, runtimeID, category); err == nil {
		s.cache[runtimeID][category] = existing
		s.rebuildCorpus(runtimeID, category, existing)
	}
}

func (s *FileLongTermStore) appendToCache(runtimeID string, category MemoryCategory, entry *MemoryEntry) {
	if _, ok := s.cache[runtimeID]; !ok {
		s.cache[runtimeID] = make(map[MemoryCategory][]*MemoryEntry)
	}
	s.cache[runtimeID][category] = append(s.cache[runtimeID][category], entry)
}

func (s *FileLongTermStore) setCache(runtimeID string, category MemoryCategory, entries []*MemoryEntry) {
	if _, ok := s.cache[runtimeID]; !ok {
		s.cache[runtimeID] = make(map[MemoryCategory][]*MemoryEntry)
	}
	s.cache[runtimeID][category] = entries
}

// --- corpus helpers (per-runtime per-category BM25 stats) ---

func (s *FileLongTermStore) getCorpus(runtimeID string, category MemoryCategory) *textsearch.CorpusStats {
	if _, ok := s.corpora[runtimeID]; !ok {
		s.corpora[runtimeID] = make(map[MemoryCategory]*textsearch.CorpusStats)
	}
	cs, ok := s.corpora[runtimeID][category]
	if !ok {
		cs = textsearch.NewCorpusStats()
		s.corpora[runtimeID][category] = cs
	}
	return cs
}

func (s *FileLongTermStore) rebuildCorpus(runtimeID string, category MemoryCategory, entries []*MemoryEntry) {
	cs := textsearch.NewCorpusStats()
	for _, e := range entries {
		cs.AddDocument(s.tokenizeEntry(e))
	}
	if _, ok := s.corpora[runtimeID]; !ok {
		s.corpora[runtimeID] = make(map[MemoryCategory]*textsearch.CorpusStats)
	}
	s.corpora[runtimeID][category] = cs
}

func (s *FileLongTermStore) tokenizeEntry(e *MemoryEntry) []string {
	text := e.Content + " " + strings.Join(e.Keywords, " ")
	tokenizer := textsearch.DetectTokenizer(text)
	return tokenizer.Tokenize(text)
}

// TimeDecay returns an exponential decay factor based on how old the entry is.
// The half-life varies by category: stable facts (profile) decay very slowly,
// while transient facts (events) decay quickly.
func TimeDecay(updatedAt time.Time, category MemoryCategory, now time.Time) float64 {
	days := now.Sub(updatedAt).Hours() / 24
	if days < 0 {
		days = 0
	}
	halfLife := CategoryHalfLife(category)
	return math.Pow(0.5, days/halfLife)
}

// CategoryHalfLife returns the BM25 time-decay half-life (in days) for a
// memory category. Unknown categories default to 90 days.
func CategoryHalfLife(cat MemoryCategory) float64 {
	switch cat {
	case CategoryProfile, CategoryPreferences:
		return 365
	case CategoryEntities:
		return 90
	case CategoryEvents:
		return 30
	case CategoryCases:
		return 60
	case CategoryPatterns:
		return 180
	default:
		return 90
	}
}
