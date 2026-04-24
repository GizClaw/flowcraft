package pipeline

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/GizClaw/flowcraft/sdk/embedding"
)

// EmbedQuery converts Request.QueryText to Request.QueryVector with a small TTL cache
// . Reads: Request.QueryText. Writes: Request.QueryVector.
//
// If Request.QueryVector is already non-empty, EmbedQuery is a no-op.
//
// The cache evicts entries via TTL expiry on read (lazy) and via an
// upper-bound eviction (MaxEntries, default 1024) on write so a long-lived
// EmbedQuery cannot accumulate unbounded memory under high-cardinality
// query traffic.
type EmbedQuery struct {
	Embedder embedding.Embedder
	TTL      time.Duration
	// MaxEntries caps the in-memory cache size. Zero means use the
	// default (1024); negative disables caching entirely.
	MaxEntries int

	mu    sync.Mutex
	cache map[string]embedCacheEntry
}

const defaultEmbedCacheMax = 1024

type embedCacheEntry struct {
	vec []float32
	exp time.Time
}

// Name implements Stage.
func (s *EmbedQuery) Name() string { return "EmbedQuery" }

// Run implements Stage.
func (s *EmbedQuery) Run(ctx context.Context, st *State) error {
	if s.Embedder == nil || st.Request == nil {
		return nil
	}
	if len(st.Request.QueryVector) > 0 || strings.TrimSpace(st.Request.QueryText) == "" {
		return nil
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	maxEntries := s.MaxEntries
	switch {
	case maxEntries < 0:
		// Caching disabled by the caller.
		v, err := s.Embedder.Embed(ctx, st.Request.QueryText)
		if err != nil {
			return err
		}
		st.Request.QueryVector = v
		return nil
	case maxEntries == 0:
		maxEntries = defaultEmbedCacheMax
	}
	key := st.Request.QueryText
	s.mu.Lock()
	if s.cache == nil {
		s.cache = make(map[string]embedCacheEntry)
	}
	if e, ok := s.cache[key]; ok {
		if time.Now().Before(e.exp) {
			st.Request.QueryVector = append([]float32(nil), e.vec...)
			s.mu.Unlock()
			return nil
		}
		// Drop the expired entry while we hold the lock to keep the
		// map size honest under high-churn workloads.
		delete(s.cache, key)
	}
	s.mu.Unlock()
	v, err := s.Embedder.Embed(ctx, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if len(s.cache) >= maxEntries {
		// Evict any single expired entry first, then fall back to a
		// random victim. This is intentionally cheap: the cache is a
		// best-effort latency optimisation, not a correctness boundary.
		now := time.Now()
		evicted := false
		for k, e := range s.cache {
			if !now.Before(e.exp) {
				delete(s.cache, k)
				evicted = true
				break
			}
		}
		if !evicted {
			for k := range s.cache {
				delete(s.cache, k)
				break
			}
		}
	}
	s.cache[key] = embedCacheEntry{vec: append([]float32(nil), v...), exp: time.Now().Add(ttl)}
	s.mu.Unlock()
	st.Request.QueryVector = v
	return nil
}

// QueryRewrite expands Request.QueryText into QueryVariants via an optional Rewriter
// . Reads: Request.QueryText. Writes: QueryVariants.
//
// If Rewriter is nil, QueryRewrite is a no-op.
type QueryRewrite struct {
	Rewriter func(ctx context.Context, text string) ([]string, error)
}

// Name implements Stage.
func (s QueryRewrite) Name() string { return "QueryRewrite" }

// Run implements Stage.
func (s QueryRewrite) Run(ctx context.Context, st *State) error {
	if s.Rewriter == nil || st.Request == nil {
		return nil
	}
	out, err := s.Rewriter(ctx, st.Request.QueryText)
	if err != nil {
		return err
	}
	st.QueryVariants = out
	return nil
}

// EntityExtract extracts proper nouns / quoted strings / compound nouns from
// the query text using a lightweight rule-based extractor by default
// . Reads: Request.QueryText. Writes: QueryEntities.
//
// Provide LLMExtractor to override with an LLM-based extractor.
type EntityExtract struct {
	LLMExtractor func(ctx context.Context, text string) ([]string, error)
}

// Name implements Stage.
func (s EntityExtract) Name() string { return "EntityExtract" }

// Run implements Stage.
func (s EntityExtract) Run(ctx context.Context, st *State) error {
	if st.Request == nil {
		return nil
	}
	if s.LLMExtractor != nil {
		out, err := s.LLMExtractor(ctx, st.Request.QueryText)
		if err != nil {
			return err
		}
		st.QueryEntities = dedupStringsLower(out)
		return nil
	}
	st.QueryEntities = ruleEntities(st.Request.QueryText)
	return nil
}

// ruleEntities is a zero-dependency entity hint extractor: capitalized tokens,
// quoted runs, CJK 2+ char compounds. Lowercase normalized.
func ruleEntities(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	set := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
		if len(s) < 2 {
			return
		}
		set[strings.ToLower(s)] = struct{}{}
	}
	for _, q := range extractQuoted(text) {
		add(q)
	}
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || (unicode.IsPunct(r) && r != '\'' && r != '-')
	}) {
		runes := []rune(w)
		if len(runes) >= 2 && unicode.IsUpper(runes[0]) {
			add(w)
		}
		if hasCJK(w) && len([]rune(w)) >= 2 {
			add(w)
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func extractQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	for _, r := range s {
		switch r {
		case '"', '\u201c', '\u201d', '\u300c', '\u300d':
			if in {
				if cur.Len() > 0 {
					out = append(out, cur.String())
				}
				cur.Reset()
				in = false
			} else {
				in = true
			}
		default:
			if in {
				cur.WriteRune(r)
			}
		}
	}
	return out
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

func dedupStringsLower(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" {
			continue
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
