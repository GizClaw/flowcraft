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
	// snapshot caches the catalog-derived half of the visible set. Guarded by
	// mu; catalogSnapshotLocked is the only reader and writer.
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
	v.mu.Lock()
	out := v.visibleDefinitionsLocked()
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
	v.mu.Lock()
	defer v.mu.Unlock()
	snap := v.catalogSnapshotLocked()
	st := fitDiscoveryPool(v.st.snapshot(), snap.sizes, v.policy.Discovery)
	for _, c := range snap.cands {
		if c.name == name {
			return include(c, st, v.policy)
		}
	}
	return false
}

// visibleDefinitionsLocked computes the definitions that reach the model for the
// current state and policy, applying the discovery pool cap and the per-round budget.
// Definitions and Discover share it so the exposure report can never drift from the
// definitions that are actually sent. Callers hold mu.
func (v *View) visibleDefinitionsLocked() []message.ToolDefinition {
	snap := v.catalogSnapshotLocked()
	st := fitDiscoveryPool(v.st.snapshot(), snap.sizes, v.policy.Discovery)
	return visibleDefinitions(snap.cands, st, v.policy)
}

// catalogSnapshotLocked returns the catalog-derived half of the visible set,
// rebuilding it only when the catalog's definition epoch moved. The epoch is read, the
// definitions are measured, and the result is published only when the epoch is still
// the one the definitions belong to: a derivation of an older catalog published under a
// newer epoch would keep stale definitions — and the byte sizes the discovery pool is
// accounted against — for as long as the catalog stood still. Callers hold mu.
func (v *View) catalogSnapshotLocked() catalogSnapshot {
	aware, keyed := v.catalog.(definitionEpoch)
	var epoch uint64
	if keyed {
		epoch = aware.DefinitionsEpoch()
		if v.snapshot.keyed && v.snapshot.cands != nil && v.snapshot.epoch == epoch {
			return v.snapshot
		}
	}
	defs := v.catalog.Definitions()
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
			exp:  v.policy.exposureOf(def.Name),
		})
	}
	if !keyed || aware.DefinitionsEpoch() == epoch {
		v.snapshot = snap
	}
	return snap
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
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bumpEpoch()

	// Sizes come from the catalog's definitions, which is what the pool and the
	// per-round budget are measured against: a deployment that rewrote a description
	// changed the bytes on the wire, and measuring the tool's own text here would make
	// the two disagree. The snapshot is shared with Definitions, so both budget
	// computations use the same figures.
	sizes := v.catalogSnapshotLocked().sizes

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
		for _, def := range v.visibleDefinitionsLocked() {
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
	v.mu.Lock()
	defer v.mu.Unlock()

	switch v.policy.exposureOf(call.Name) {
	case ExposureDirect, ExposureDeferred:
	default:
		return
	}
	t, ok := v.catalog.Get(call.Name)
	if !ok {
		return
	}
	size, measured := v.catalogSnapshotLocked().sizes[call.Name]
	if !measured {
		size = definitionBytes(t.Definition())
	}
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
