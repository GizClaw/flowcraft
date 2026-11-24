package projection

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// View is one session's discovery state: what the model may see this round, what it
// has found, and what it used recently.
//
// It implements [tool.View], so a host attaches it to a [tool.Binding] with
// Binding.WithView and every tool that dispatches sees the same projection the model
// does. A host creates one per session and keeps it for the session's life.
//
// A View is safe for concurrent use.
type View struct {
	catalog    tool.Catalog
	projection *Projection
	policy     Policy

	mu sync.Mutex
	st viewState
	// epoch bumps on every mutation that can change the visible set.
	epoch uint64
	// snapshot caches the catalog-derived half of the visible set for one catalog
	// definition epoch. Guarded by mu; cachedSnapshot and publishSnapshot are the
	// only reader and writer, and a build never runs under mu.
	snapshot catalogSnapshot
	// searchCache is the process-wide BM25 index. Nil for a view built by hand
	// (tests, a fixture): such a view keeps no index between calls.
	searchCache *searchIndexCache
}

// catalogSnapshot is the part of the visible set that depends only on the catalog's
// definitions: the candidates (name, definition, exposure — the exposure comes from
// the policy, which is fixed after construction) and the serialized size of each
// definition. The discovery pool and the per-round budget are state-dependent and are
// applied per call.
//
// A published snapshot is immutable: concurrent readers share its slice and its map, so
// whoever built it must not touch either after handing it over.
type catalogSnapshot struct {
	cands []candidate
	sizes map[string]int64

	epoch uint64
	keyed bool
}

// Epoch changes whenever the visible set could have changed: a discovery, a recorded
// call, an advanced round, a load. A caller that caches anything derived from
// Definitions (a toolset, a byte budget) can reuse it while the epoch is unchanged.
func (v *View) Epoch() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.epoch
}

// bumpEpoch must be called with mu held.
func (v *View) bumpEpoch() { v.epoch++ }

// Definitions implements [tool.View]: the definitions that reach the model this
// round, after the discovery pool cap and the per-round budget.
func (v *View) Definitions() []message.ToolDefinition {
	snap := v.catalogSnapshot()
	v.mu.Lock()
	out := v.visibleDefinitionsLocked(snap)
	v.mu.Unlock()
	return out
}

// Visible implements [tool.View].
//
// It reports whether the name is eligible this round — exposed by policy, required, or
// in the discovery pool. The per-round budget is applied when the definitions are
// actually sent, so a tool that is eligible but loses the budget still answers true
// here; authority is a separate question that dispatch answers, not the projection.
func (v *View) Visible(name string) bool {
	snap := v.catalogSnapshot()
	v.mu.Lock()
	defer v.mu.Unlock()
	st := fitDiscoveryPool(v.st.snapshot(), snap.sizes, v.policy.Discovery)
	for _, c := range snap.cands {
		if c.name == name {
			return include(c, st, v.policy)
		}
	}
	return false
}

// visibleDefinitionsLocked computes the definitions that reach the model for the given
// catalog snapshot and the current state and policy, applying the discovery pool cap and
// the per-round budget. Definitions and Discover share it so the exposure report can
// never drift from the definitions that are actually sent. Callers hold mu.
func (v *View) visibleDefinitionsLocked(snap catalogSnapshot) []message.ToolDefinition {
	st := fitDiscoveryPool(v.st.snapshot(), snap.sizes, v.policy.Discovery)
	return visibleDefinitions(snap.cands, st, v.policy)
}

// catalogSnapshotAttempts bounds the rebuild loop in catalogSnapshot. One retry is all
// the loop is for — the catalog moved once while a snapshot was being built — and a
// catalog that never stands still must not keep a reader from returning: the last build
// answers the call without being published, which is what a reader pays on a miss anyway.
const catalogSnapshotAttempts = 2

// catalogSnapshot returns the catalog-derived half of the visible set for the catalog's
// current definition epoch.
//
// The epoch is read, the snapshot is built with no lock held, and it is published only
// while the epoch is still the one its definitions belong to — the shape the registry's
// own Definitions cache uses. Building it under mu meant a catalog re-list ran in front
// of every concurrent call in the session, and a round records its calls from several
// goroutines ([tool.Dispatcher.ExecuteAll]); the lock now covers only the state half.
func (v *View) catalogSnapshot() catalogSnapshot {
	for attempt := 1; ; attempt++ {
		epoch, keyed := catalogEpoch(v.catalog)
		if snap, ok := v.cachedSnapshot(epoch, keyed); ok {
			return snap
		}
		snap := buildCatalogSnapshot(v.catalog, v.policy, epoch, keyed)
		if v.publishSnapshot(snap) || attempt >= catalogSnapshotAttempts {
			return snap
		}
		// The catalog moved while the snapshot was being built. Publishing it would
		// keep definitions — and the byte sizes the discovery pool is accounted
		// against — under an epoch they do not belong to, so the build is dropped and
		// the next pass starts from the newer epoch.
	}
}

// cachedSnapshot returns the snapshot already built for the catalog's definition epoch,
// if the cache holds one. It never builds: a miss is the caller's cue to build with no
// lock held.
func (v *View) cachedSnapshot(epoch uint64, keyed bool) (catalogSnapshot, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if keyed && v.snapshot.keyed && v.snapshot.cands != nil && v.snapshot.epoch == epoch {
		return v.snapshot, true
	}
	return catalogSnapshot{}, false
}

// publishSnapshot records snap as the cached snapshot and reports whether it did.
//
// A build that lost the race is not published: caching a derivation of an older catalog
// under a newer epoch would keep stale definitions — and the byte sizes the discovery
// pool is accounted against — for as long as the catalog stood still. The epoch is read
// before the lock, so a move in the window can still land a snapshot tagged with the
// epoch it was built for; readers compare that tag, so such a store costs one rebuild
// rather than a wrong answer.
func (v *View) publishSnapshot(snap catalogSnapshot) bool {
	epoch, keyed := catalogEpoch(v.catalog)
	if !keyed || epoch != snap.epoch {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.snapshot = snap
	return true
}

// buildCatalogSnapshot derives the catalog half of the visible set from the catalog's
// definitions. It runs with no view lock held: Definitions reaches a deferred tool's own
// lock, a decorator that is caller code, or a read-side wrapper, and none of that may run
// in front of another goroutine's RecordCall.
func buildCatalogSnapshot(catalog tool.Catalog, policy Policy, epoch uint64, keyed bool) catalogSnapshot {
	defs := catalog.Definitions()
	snap := catalogSnapshot{
		cands: make([]candidate, 0, len(defs)),
		sizes: make(map[string]int64, len(defs)),
		epoch: epoch,
		keyed: keyed,
	}
	for _, def := range defs {
		snap.sizes[def.Name] = definitionBytes(def)
		snap.cands = append(snap.cands, candidate{
			name: def.Name,
			def:  def,
			exp:  policy.exposureOf(def.Name),
		})
	}
	return snap
}

// catalogEpoch reports the catalog's definition epoch and whether the catalog can say.
func catalogEpoch(catalog tool.Catalog) (uint64, bool) {
	aware, ok := catalog.(definitionEpoch)
	if !ok {
		return 0, false
	}
	return aware.DefinitionsEpoch(), true
}

// fitDiscoveryPool restricts the snapshot's discovered set to the pool budget,
// measured against the live definitions of the current round. Most-recently-used
// entries stay first; required tools are unaffected (they live in their own set). The
// read path never mutates state, so entries trimmed here are evicted for real on the
// next mutation or AdvanceRound. Like the per-round budget, at least one entry is kept
// so an oversized single tool remains discoverable.
func fitDiscoveryPool(st stateSnapshot, sizes map[string]int64, pool DiscoveryPolicy) stateSnapshot {
	if len(st.discovered) == 0 {
		return st
	}
	names := make([]string, 0, len(st.discovered))
	for name := range st.discovered {
		if _, ok := sizes[name]; ok {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := st.discovered[names[i]], st.discovered[names[j]]
		if a.lastUse != b.lastUse {
			return a.lastUse > b.lastUse
		}
		if a.seq != b.seq {
			return a.seq > b.seq
		}
		return names[i] < names[j]
	})

	var total int64
	kept := make(map[string]discoveredEntry, len(names))
	for _, name := range names {
		if pool.MaxTools > 0 && len(kept) >= pool.MaxTools {
			break
		}
		size := sizes[name]
		if total+size > pool.MaxBytes && len(kept) > 0 {
			break
		}
		total += size
		kept[name] = st.discovered[name]
	}
	out := st
	out.discovered = kept
	return out
}

// Require adds names to the required set: they are visible every round regardless of
// discovery, and the model can call them at any time. Idempotent.
func (v *View) Require(names ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.st.require(names...)
	v.bumpEpoch()
}

// Discover adds or refreshes discovery entries. Each requested name must already be
// loaded (tool_search calls EnsureLoaded first) so the pool measures real definitions;
// entries are then evicted LRU until the pool fits Policy.Discovery.
func (v *View) Discover(names ...string) DiscoverOutcome {
	// The snapshot is read before the lock: measuring the definitions is the
	// catalog's cost, and it must not be paid in front of every other call in the
	// session.
	snap := v.catalogSnapshot()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bumpEpoch()

	// Sizes come from the catalog's definitions, which is what the pool and the
	// per-round budget are measured against: a deployment that rewrote a description
	// changed the bytes on the wire, and measuring the tool's own text here would make
	// the two disagree. The snapshot is shared with Definitions, so both budget
	// computations use the same figures.
	sizes := snap.sizes

	reasons := make(map[string]string, len(names))
	changed := false
	rank := 0
	for _, name := range names {
		t, ok := v.catalog.Get(name)
		if !ok {
			reasons[name] = "unknown"
			continue
		}
		switch v.policy.exposureOf(name) {
		case ExposureDirect, ExposureDeferred:
		default:
			reasons[name] = "not_discoverable"
			continue
		}
		size, measured := sizes[name]
		if !measured {
			size = definitionBytes(t.Definition())
		}
		// names is ranked: rank 0 is the best hit, and the per-round visible budget
		// keeps the smaller ranks when it has to cut.
		v.st.touch(name, size, rank)
		rank++
		changed = true
	}

	var evicted []string
	if changed {
		evicted = v.evictForFitLocked()
	}
	// Report what the next Definitions call will send, not just what the pool
	// remembers: a freshly discovered name that loses the per-round budget must not be
	// announced as exposed.
	var visible map[string]struct{}
	if changed {
		visible = make(map[string]struct{})
		for _, def := range v.visibleDefinitionsLocked(snap) {
			visible[def.Name] = struct{}{}
		}
	}
	results := make([]DiscoverResult, 0, len(names))
	for _, name := range names {
		result := DiscoverResult{Name: name}
		if reason := reasons[name]; reason != "" {
			// Unknown or undiscoverable names keep their own reason even when an older
			// pool entry still exists.
			result.Reason = reason
		} else if _, present := v.st.discovered[name]; !present {
			result.Reason = "over_budget"
		} else if _, ok := visible[name]; !ok {
			result.Reason = "visible_budget"
		} else {
			result.Exposed = true
		}
		results = append(results, result)
	}
	return DiscoverOutcome{Results: results, Evicted: evicted}
}

// evictForFitLocked removes least-recently-used discovery entries until the pool fits
// MaxTools and MaxBytes. Between equally recent entries, Deferred tools are evicted
// before Direct ones; a single oversized entry is kept so discovery cannot dead-end on
// one big schema. Callers hold mu.
func (v *View) evictForFitLocked() []string {
	pool := v.policy.Discovery
	var evicted []string
	for {
		tools, total := v.st.discoveredTotals()
		if tools <= pool.MaxTools && (total <= pool.MaxBytes || tools <= 1) {
			return evicted
		}
		if tools == 0 {
			return evicted
		}

		var victim string
		var victimEntry discoveredEntry
		victimDeferred := false
		first := true
		for name, entry := range v.st.discovered {
			deferred := v.policy.exposureOf(name) == ExposureDeferred
			if first || evictsBefore(entry, deferred, victimEntry, victimDeferred) {
				victim = name
				victimEntry = entry
				victimDeferred = deferred
				first = false
			}
		}
		v.st.removeDiscovered(victim)
		evicted = append(evicted, victim)
	}
}

// evictsBefore reports whether a should be evicted before b: least recently used
// first; on recency ties Deferred before Direct; then oldest discovery sequence.
func evictsBefore(a discoveredEntry, aDeferred bool, b discoveredEntry, bDeferred bool) bool {
	if a.lastUse != b.lastUse {
		return a.lastUse < b.lastUse
	}
	if aDeferred != bDeferred {
		return aDeferred
	}
	return a.seq < b.seq
}

// RecordCall implements [tool.View]: a tool the model used stays in the discovery pool
// and becomes the most recently used entry.
func (v *View) RecordCall(call message.ToolCall) {
	// Everything the pool entry needs is read before the lock: the policy is fixed
	// after construction, and a session records one call per result, so neither a
	// re-list nor a catalog read belongs in the way of the next goroutine's call.
	switch v.policy.exposureOf(call.Name) {
	case ExposureDirect, ExposureDeferred:
	default:
		return
	}
	t, ok := v.catalog.Get(call.Name)
	if !ok {
		return
	}
	snap := v.catalogSnapshot()
	size, measured := snap.sizes[call.Name]
	if !measured {
		size = definitionBytes(t.Definition())
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.st.touch(call.Name, size, 0)
	v.bumpEpoch()
	v.evictForFitLocked()
}

// AdvanceRound moves to the next round, expiring discovery entries that went idle and
// dropping names no longer in the catalog. Call it once per inference round.
func (v *View) AdvanceRound() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bumpEpoch()

	alive := func(name string) bool {
		_, ok := v.catalog.Get(name)
		return ok
	}
	v.st.advanceTurn(v.policy.Discovery.IdleRounds, alive)
	// Definitions only trims overflow in its read view; re-enforce the pool cap on
	// state here so entries dropped by the view are really evicted even when no
	// further Discover/RecordCall happens.
	v.evictForFitLocked()
}

// Search ranks the searchable definitions against query with BM25. It never loads
// deferred tools unless the policy says to.
func (v *View) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if v.policy.SearchWithLoad {
		if err := v.EnsureLoaded(ctx, v.loadableNames()...); err != nil {
			telemetry.WarnErr(ctx, "tool view: preload for search failed", err)
		}
	}
	return v.search(ctx, query, limit)
}

// SearchWithLoad loads every deferred tool before ranking, so hits are computed over
// real metadata. It is the explicit opt-in for hosts that need the complete tool set;
// tool_search itself follows Policy.SearchWithLoad.
func (v *View) SearchWithLoad(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if err := v.EnsureLoaded(ctx, v.loadableNames()...); err != nil {
		telemetry.WarnErr(ctx, "tool view: preload for search failed", err)
	}
	return v.search(ctx, query, limit)
}

func (v *View) search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	v.mu.Lock()
	policy := v.policy
	v.mu.Unlock()

	// Every hit is loaded and takes a discovery-pool slot, so a model-supplied limit
	// cannot exceed the pool it would be loaded into: a larger one only churns the pool
	// with hits that can never be exposed.
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if pool := policy.Discovery.MaxTools; pool > 0 && limit > pool {
		limit = pool
	}
	index, err := v.index(policy)
	if err != nil {
		return nil, err
	}
	return index.search(query, limit), nil
}

// index returns the BM25 index over the searchable definitions. Building it tokenizes
// the whole catalog, so the projection keeps one and reuses it until the catalog's
// definitions change; a view without a cache rebuilds every call.
func (v *View) index(policy Policy) (*searchIndex, error) {
	build := func() []searchDoc {
		defs := v.catalog.Definitions()
		searchable := make([]message.ToolDefinition, 0, len(defs))
		for _, def := range defs {
			if policy.exposureOf(def.Name).searchable() {
				searchable = append(searchable, def)
			}
		}
		return searchDocs(searchable)
	}
	if v.searchCache == nil {
		return buildSearchIndex(build())
	}
	return v.searchCache.get(v.catalog, build)
}

// loadableNames lists every definition name, so a preload reaches each lazy tool.
func (v *View) loadableNames() []string {
	defs := v.catalog.Definitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
}

// EnsureLoaded forces the deferred load of the named tools so the next Definitions call
// sees their real schemas. Unknown or already-concrete tools are skipped.
func (v *View) EnsureLoaded(ctx context.Context, names ...string) error {
	var errs []error
	for _, name := range names {
		t, ok := v.catalog.Get(name)
		if !ok {
			continue
		}
		if lazy, ok := t.(interface{ EnsureLoaded(context.Context) error }); ok {
			if err := lazy.EnsureLoaded(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// searchBudgetBytes is the byte budget one tool_search answer may spend: the same
// Budget.MaxBytes that bounds this round's definitions.
func (v *View) searchBudgetBytes() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.policy.Budget.MaxBytes
}
