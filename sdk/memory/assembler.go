package memory

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
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
}

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
	return &ContextAssembler{
		ltStore: ltStore,
		config:  config,
		cache:   newAssemblerCache(),
	}
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

	// Pinned layer: runtime-global bucket when scope is enabled.
	var pinnedScope *MemoryScope
	if a.config.ScopeEnabled {
		pinnedScope = &MemoryScope{RuntimeID: runtimeID}
	}

	var pinned []*MemoryEntry
	for _, cat := range pinCats {
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

	query := extractQueryFromMessages(msgs)
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
			seenIDs := make(map[string]bool, len(pinned))
			for _, e := range pinned {
				seenIDs[e.ID] = true
			}
			for _, cat := range recCats {
				results, err := a.searchRecallCategory(ctx, runtimeID, query, cat, perCatLimit, searchScope, a.recallPartitionsFor(cat))
				if err != nil {
					telemetry.Warn(ctx, "assembler: recall search failed",
						otellog.String("runtime_id", runtimeID),
						otellog.String("category", string(cat)),
						otellog.String("error", err.Error()))
					continue
				}
				for _, e := range results {
					if !seenIDs[e.ID] {
						seenIDs[e.ID] = true
						recalled = append(recalled, e)
					}
				}
			}
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

func (a *ContextAssembler) searchRecallCategory(ctx context.Context, runtimeID, query string, cat MemoryCategory, topK int, scope *MemoryScope, partitions []MemoryPartition) ([]*MemoryEntry, error) {
	rec := a.recallScopeFromScope(runtimeID, scope, partitions)
	cacheKey := recallCacheKey(runtimeID, scope, rec, string(cat), query)
	if entries, ok := a.cache.readRecall(cacheKey); ok {
		recordAssemblerCacheHit(ctx, "recall")
		return entries, nil
	}
	recordAssemblerCacheMiss(ctx, "recall")
	entries, err := a.ltStore.Search(ctx, runtimeID, query, SearchOptions{
		Category: cat,
		TopK:     topK,
		Recall:   rec,
	})
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
