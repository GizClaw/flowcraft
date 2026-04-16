package memory

import (
	"context"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/sync/errgroup"
)

// ContextAssembler loads and formats long-term memory for system prompt injection.
type ContextAssembler struct {
	ltStore LongTermStore
	config  AssemblerConfig
	cache   *assemblerCache
}

// AssemblerConfig controls pinned vs recall behavior and cache TTLs.
type AssemblerConfig struct {
	MaxEntries       int
	PinnedCategories []MemoryCategory
	RecallCategories []MemoryCategory
	// RecallPartitions maps category → partition union for recall Search/List.
	// nil or absent keys default to user-only.
	RecallPartitions map[MemoryCategory][]MemoryPartition
	PinnedCacheTTL   time.Duration
	RecallCacheTTL   time.Duration
	ScopeEnabled     bool
	GlobalCategories []MemoryCategory
	// MaxRecallConcurrency limits the number of recall category searches that
	// run in parallel. Zero or negative values default to defaultMaxRecallConcurrency.
	MaxRecallConcurrency int
}

const defaultMaxRecallConcurrency = 4

// NewContextAssembler builds an assembler. Zero TTLs default to aware.go constants.
func NewContextAssembler(ltStore LongTermStore, config AssemblerConfig) *ContextAssembler {
	if config.PinnedCacheTTL <= 0 {
		config.PinnedCacheTTL = pinnedCacheTTL
	}
	if config.RecallCacheTTL <= 0 {
		config.RecallCacheTTL = recallCacheTTL
	}
	if config.MaxEntries <= 0 {
		config.MaxEntries = 10
	}
	if config.MaxRecallConcurrency <= 0 {
		config.MaxRecallConcurrency = defaultMaxRecallConcurrency
	}
	return &ContextAssembler{
		ltStore: ltStore,
		config:  config,
		cache:   newAssemblerCache(),
	}
}

// embedPreWarmer is an optional interface satisfied by stores (e.g. HybridStore)
// that can pre-compute query embeddings in parallel with pinned loading.
type embedPreWarmer interface {
	EmbedQuery(ctx context.Context, query string) ([]float32, error)
}

// searchOption applies optional overrides to a SearchOptions value.
type searchOption func(*SearchOptions)

func withQueryVector(v []float32) searchOption {
	return func(o *SearchOptions) { o.QueryVector = v }
}

// Assemble returns formatted long-term memory text, or empty string if none.
func (a *ContextAssembler) Assemble(ctx context.Context, runtimeID string, scope *MemoryScope, msgs []model.Message) (string, error) {
	if a.ltStore == nil {
		return "", nil
	}

	pinCats := a.config.PinnedCategories
	if len(pinCats) == 0 {
		pinCats = defaultPinnedCategories
	}
	recCats := a.config.RecallCategories
	if len(recCats) == 0 {
		recCats = defaultRecallCategories
	}

	maxEntries := a.config.MaxEntries
	query := extractQueryFromMessages(msgs)

	// ── Parallel: pinned loading ∥ embedding pre-computation ──

	type embedResult struct {
		vec []float32
		err error
	}
	embedCh := make(chan embedResult, 1)
	go func() {
		if query == "" || len(recCats) == 0 {
			embedCh <- embedResult{}
			return
		}
		pw, ok := a.ltStore.(embedPreWarmer)
		if !ok {
			embedCh <- embedResult{}
			return
		}
		v, e := pw.EmbedQuery(ctx, query)
		embedCh <- embedResult{vec: v, err: e}
	}()

	// Pinned layer: runtime-global bucket when scope is enabled.
	var pinnedScope *MemoryScope
	if a.config.ScopeEnabled {
		pinnedScope = &MemoryScope{RuntimeID: runtimeID}
	}

	var pinned []*MemoryEntry
	for _, cat := range pinCats {
		if ctx.Err() != nil {
			break
		}
		entries, err := a.loadPinnedCategory(ctx, runtimeID, cat, pinnedScope)
		if err != nil {
			telemetry.Warn(ctx, "assembler: list pinned category failed",
				otellog.String("runtime_id", runtimeID),
				otellog.String("category", string(cat)),
				otellog.String("error", err.Error()))
			continue
		}
		pinned = append(pinned, entries...)
	}

	// ── Join: wait for embedding result ──
	er := <-embedCh
	if er.err != nil {
		telemetry.Warn(ctx, "assembler: pre-embed failed",
			otellog.String("runtime_id", runtimeID),
			otellog.String("error", er.err.Error()))
	}

	// ── Recall phase: queryVec is ready, pass through SearchOptions ──
	var recalled []*MemoryEntry
	if query != "" && len(recCats) > 0 {
		recallScopeKey := runtimeID
		var searchScope *MemoryScope
		if a.config.ScopeEnabled {
			if scope != nil {
				sc := *scope
				sc.RuntimeID = runtimeID
				searchScope = &sc
				recallScopeKey = sc.CacheKey()
			} else {
				searchScope = &MemoryScope{RuntimeID: runtimeID}
			}
		}

		recallReuse := false
		if a.canReuseRecall(recallScopeKey, query) {
			snap, ok := a.cache.getLastRecall(recallScopeKey)
			if ok {
				recordAssemblerCacheHit(ctx, "recall_reuse")
				recalled = snap.result
				recallReuse = true
			}
		}
		if !recallReuse {
			recallBudget := maxEntries - len(pinned)
			if recallBudget <= 0 {
				recallBudget = 3
			}
			perCatLimit := recallBudget/len(recCats) + 1

			var recallOpts []searchOption
			if er.vec != nil {
				recallOpts = append(recallOpts, withQueryVector(er.vec))
			}

			recalled = a.searchRecallParallel(ctx, runtimeID, query, recCats, perCatLimit, searchScope, recallOpts)

			// Dedup against pinned entries.
			seenIDs := make(map[string]bool, len(pinned))
			for _, e := range pinned {
				seenIDs[e.ID] = true
			}
			n := 0
			for _, e := range recalled {
				if !seenIDs[e.ID] {
					seenIDs[e.ID] = true
					recalled[n] = e
					n++
				}
			}
			recalled = recalled[:n]

			if len(recalled) > recallBudget {
				recalled = recalled[:recallBudget]
			}
			a.cache.setLastRecall(recallScopeKey, query, recalled)
		}
	}

	all := mergeAndDedupEntries(pinned, recalled, maxEntries)
	if len(all) == 0 {
		return "", nil
	}
	return formatLongTermMemory(all), nil
}

func (a *ContextAssembler) loadPinnedCategory(ctx context.Context, runtimeID string, cat MemoryCategory, scope *MemoryScope) ([]*MemoryEntry, error) {
	cacheKey := runtimeID + "|pin|" + string(cat)
	if scope != nil {
		cacheKey = scope.CacheKey() + "|pin|" + string(cat)
	}
	if entries, ok := a.cache.readPinned(cacheKey); ok {
		recordAssemblerCacheHit(ctx, "pinned")
		return entries, nil
	}
	recordAssemblerCacheMiss(ctx, "pinned")
	entries, err := a.ltStore.List(ctx, runtimeID, ListOptions{Category: cat, Limit: pinnedLimitPerCategory, Scope: scope})
	if err != nil {
		return nil, err
	}
	a.cache.writePinned(cacheKey, entries, a.config.PinnedCacheTTL)
	return entries, nil
}

func (a *ContextAssembler) recallPartitionsFor(cat MemoryCategory) []MemoryPartition {
	if len(a.config.RecallPartitions) == 0 {
		return []MemoryPartition{PartitionUser}
	}
	if p, ok := a.config.RecallPartitions[cat]; ok && len(p) > 0 {
		return NormalizePartitions(p)
	}
	return []MemoryPartition{PartitionUser}
}

// searchRecallParallel searches all recall categories concurrently, bounded by
// MaxRecallConcurrency. Results are collected in category order and de-duped
// across categories (but not yet against pinned — caller handles that).
func (a *ContextAssembler) searchRecallParallel(
	ctx context.Context, runtimeID, query string,
	cats []MemoryCategory, perCatLimit int,
	scope *MemoryScope, opts []searchOption,
) []*MemoryEntry {
	type indexedResult struct {
		idx     int
		entries []*MemoryEntry
	}

	concurrency := a.config.MaxRecallConcurrency
	if concurrency > len(cats) {
		concurrency = len(cats)
	}
	sem := make(chan struct{}, concurrency)

	var mu sync.Mutex
	results := make([]indexedResult, 0, len(cats))

	g, gCtx := errgroup.WithContext(ctx)
	for i, cat := range cats {
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			entries, err := a.searchRecallCategory(gCtx, runtimeID, query, cat,
				perCatLimit, scope, a.recallPartitionsFor(cat), opts...)
			if err != nil {
				telemetry.Warn(gCtx, "assembler: recall search failed",
					otellog.String("runtime_id", runtimeID),
					otellog.String("category", string(cat)),
					otellog.String("error", err.Error()))
				return nil
			}
			mu.Lock()
			results = append(results, indexedResult{idx: i, entries: entries})
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// Stable-sort by original category order for deterministic output.
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].idx < results[j-1].idx; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	// Merge + cross-category dedup.
	seen := make(map[string]bool)
	var out []*MemoryEntry
	for _, r := range results {
		for _, e := range r.entries {
			if e != nil && !seen[e.ID] {
				seen[e.ID] = true
				out = append(out, e)
			}
		}
	}
	return out
}

func (a *ContextAssembler) searchRecallCategory(ctx context.Context, runtimeID, query string, cat MemoryCategory, topK int, scope *MemoryScope, partitions []MemoryPartition, opts ...searchOption) ([]*MemoryEntry, error) {
	rec := a.recallScopeFromScope(runtimeID, scope, partitions)
	cacheKey := recallCacheKey(runtimeID, scope, rec, string(cat), query)
	if entries, ok := a.cache.readRecall(cacheKey); ok {
		recordAssemblerCacheHit(ctx, "recall")
		return entries, nil
	}
	recordAssemblerCacheMiss(ctx, "recall")
	so := SearchOptions{
		Category: cat,
		TopK:     topK,
		Recall:   rec,
	}
	for _, fn := range opts {
		fn(&so)
	}
	entries, err := a.ltStore.Search(ctx, runtimeID, query, so)
	if err != nil {
		return nil, err
	}
	a.cache.writeRecall(cacheKey, entries, a.config.RecallCacheTTL)
	return entries, nil
}

func recallCacheKey(runtimeID string, scope *MemoryScope, rec *RecallScope, cat, query string) string {
	suffix := "|recall|" + cat + "|" + query
	if rec != nil {
		return rec.CacheKey() + suffix
	}
	if scope != nil {
		return scope.CacheKey() + suffix
	}
	return runtimeID + suffix
}

func (a *ContextAssembler) recallScopeFromScope(runtimeID string, scope *MemoryScope, partitions []MemoryPartition) *RecallScope {
	if !a.config.ScopeEnabled {
		return nil
	}
	parts := NormalizePartitions(partitions)
	if scope == nil {
		return normalizeRecall(&RecallScope{RuntimeID: runtimeID, Partitions: parts}, runtimeID)
	}
	sc := *scope
	sc.RuntimeID = runtimeID
	if sc.IsGlobal() {
		return normalizeRecall(&RecallScope{
			RuntimeID:  runtimeID,
			Partitions: parts,
		}, runtimeID)
	}
	return normalizeRecall(&RecallScope{
		RuntimeID:  runtimeID,
		UserID:     sc.UserID,
		SessionID:  sc.SessionID,
		Partitions: parts,
	}, runtimeID)
}

func (a *ContextAssembler) canReuseRecall(scopeKey, query string) bool {
	snap, ok := a.cache.getLastRecall(scopeKey)
	if !ok || len(snap.result) == 0 || snap.query == "" {
		return false
	}
	elapsed := time.Since(snap.at)
	if elapsed >= recallThrottleMaxInterval {
		return false
	}
	if elapsed < recallThrottleMinInterval {
		return true
	}
	if query == snap.query {
		return true
	}
	return queryChangeRatio(snap.query, query) < recallQueryChangeRatio
}

func mergeAndDedupEntries(pinned, recalled []*MemoryEntry, maxEntries int) []*MemoryEntry {
	seen := make(map[string]bool)
	var out []*MemoryEntry
	for _, e := range pinned {
		if e == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	for _, e := range recalled {
		if e == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	if maxEntries > 0 && len(out) > maxEntries {
		out = out[:maxEntries]
	}
	return out
}
