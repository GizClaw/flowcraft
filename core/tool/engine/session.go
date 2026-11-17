package engine

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
)

// CatalogView is the per-run / per-conversation injection view over a
// [Catalog]. An Engine creates one at run start, feeds the model
// Definitions() each round, records calls, and advances turns.
//
// When the assembly was built without dynamic injection, the view is
// static: Definitions() shows every tool, and the stateful
// operations (Require/Discover/RecordCall/AdvanceRound) are no-ops while
// Search returns NotAvailable.
type CatalogView interface {
	tool.Catalog

	// Require adds names to the RequiredByName set. Idempotent.
	Require(names ...string)
	// Discover adds names to the discovery pool (or refreshes their
	// recency). The pool is bounded by Policy.Discovery: entries are
	// evicted least-recently-used when the pool is over budget and
	// swept after idle rounds. A discovered tool must be loaded before
	// the next Definitions call, otherwise the model would see its
	// placeholder schema — tool_search loads each name before
	// discovering it. Names are ranked: the first one wins when a
	// same-round tie is pruned. DiscoverResult.Exposed reports whether
	// the name will actually appear in the next Definitions call, so a
	// pooled tool that loses the per-round budget comes back with
	// Reason "visible_budget" instead of a false positive.
	Discover(names ...string) DiscoverOutcome
	// RecordCall records that the model called name this round,
	// refreshing its discovery-pool recency. Always and Hidden tools
	// are skipped: the former are always visible, the latter must
	// never surface through use.
	RecordCall(call message.ToolCall)
	// AdvanceRound moves to the next round, expiring discovery entries
	// that went idle and dropping names no longer in the catalog. Call
	// it once per inference round.
	AdvanceRound()
	// Search ranks searchable definitions against query with BM25.
	// Search never loads deferred tools.
	Search(ctx context.Context, query string, limit int) ([]SearchHit, error)
	// SearchWithLoad loads every deferred tool before ranking, so hits
	// are computed over real metadata. It is the explicit opt-in for
	// hosts that need the complete tool set; tool_search itself follows
	// Policy.SearchWithLoad instead.
	SearchWithLoad(ctx context.Context, query string, limit int) ([]SearchHit, error)
	// Load eagerly loads every deferred tool. One failing source does
	// not stop the others; errors are joined.
	Load(ctx context.Context) error
	// EnsureLoaded forces the deferred load of the named tools so the
	// next Definitions call sees their real schemas. Unknown or
	// already-concrete tools are skipped.
	EnsureLoaded(ctx context.Context, names ...string) error
	// Epoch changes whenever the visible set could have changed: a discovery,
	// a recorded call, an advanced round, a load. A caller that caches anything
	// derived from Definitions (a toolset, a byte budget) can reuse it while the
	// epoch is unchanged.
	Epoch() uint64
}

// DiscoverResult reports one tool's discovery outcome.
type DiscoverResult struct {
	Name string `json:"name"`
	// Exposed reports whether the tool will appear in the next
	// Definitions call: pooled and within the per-round budget.
	Exposed bool `json:"exposed"`
	// Reason explains a name that is not exposed:
	// "unknown" | "not_discoverable" | "over_budget" | "visible_budget".
	Reason string `json:"reason,omitempty"`
}

// DiscoverOutcome summarizes one Discover call.
type DiscoverOutcome struct {
	// Results carries one entry per requested name, in input order.
	Results []DiscoverResult `json:"results"`
	// Evicted lists pool entries dropped by budget pressure (LRU)
	// while fitting the newly discovered names.
	Evicted []string `json:"evicted,omitempty"`
}

// dynamicView is the stateful injection view. It reads tools from
// the shared registry (the execution surface) and applies Exposure /
// discovery pool / budget purely on the read side.
type dynamicView struct {
	catalog tool.Catalog
	policy  Policy

	mu sync.Mutex
	st viewState
	// epoch bumps on every mutation that can change the visible set.
	epoch uint64
	// snapshot caches the catalog-derived half of the visible set. Guarded by
	// mu; catalogSnapshotLocked is the only reader and writer.
	snapshot catalogSnapshot
	// searchCache is the assembly-wide BM25 index. Nil for a view built by hand
	// (tests, a fixture): such a view keeps no index between calls, which is the
	// behaviour every view had before the cache existed.
	searchCache *searchIndexCache
}

// catalogSnapshot is the part of the visible set that depends only on the
// catalog's definitions: the candidates (name, definition, exposure — the
// exposure comes from the per-view policy, which is fixed after construction)
// and the serialized size of each definition. The discovery pool and the
// per-round budget are state-dependent and are applied per call.
//
// epoch is the catalog's definition epoch the snapshot was built from; keyed
// records that the catalog offers one at all. A catalog without the capability
// keeps the rebuild-per-call behaviour.
type catalogSnapshot struct {
	cands []candidate
	sizes map[string]int64

	epoch uint64
	keyed bool
}

// Epoch implements CatalogView.
func (s *dynamicView) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// bumpEpoch must be called with mu held.
func (s *dynamicView) bumpEpoch() { s.epoch++ }

func (s *dynamicView) Get(name string) (tool.Tool, bool) {
	return s.catalog.Get(name)
}

func (s *dynamicView) Definitions() []message.ToolDefinition {
	s.mu.Lock()
	out := s.visibleDefinitionsLocked()
	s.mu.Unlock()
	return out
}

// visibleDefinitionsLocked computes the definitions that reach the model for the
// current state and policy, applying the discovery pool cap and the per-round
// budget. Definitions() and Discover() share it so the exposure report can
// never drift from the definitions that are actually sent. Callers hold mu.
func (s *dynamicView) visibleDefinitionsLocked() []message.ToolDefinition {
	policy := s.policy
	snap := s.catalogSnapshotLocked()
	st := fitDiscoveryPool(s.st.snapshot(), snap.sizes, policy.Discovery)
	return visibleDefinitions(snap.cands, st, policy)
}

// catalogSnapshotLocked returns the catalog-derived half of the visible set,
// rebuilding it only when the catalog's definition epoch moved. The epoch is
// read, the definitions are measured, and the result is published only when the
// epoch is still the one the definitions belong to: a derivation of an older
// catalog published under a newer epoch would keep stale definitions — and the
// byte sizes the discovery pool is accounted against — for as long as the
// catalog stood still. Callers hold mu.
func (s *dynamicView) catalogSnapshotLocked() catalogSnapshot {
	aware, keyed := s.catalog.(definitionEpoch)
	var epoch uint64
	if keyed {
		epoch = aware.DefinitionsEpoch()
		if s.snapshot.keyed && s.snapshot.cands != nil && s.snapshot.epoch == epoch {
			return s.snapshot
		}
	}
	defs := s.catalog.Definitions()
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
			exp:  s.policy.exposureOf(def.Name),
		})
	}
	if !keyed || aware.DefinitionsEpoch() == epoch {
		s.snapshot = snap
	}
	return snap
}

// fitDiscoveryPool restricts the snapshot's discovered set to the pool
// budget, measured against the live definitions of the current round.
// Most-recently-used entries stay first; required tools are unaffected
// (they live in their own set). The read path never mutates state, so
// entries trimmed here are evicted for real on the next mutation or
// AdvanceRound. Like the per-round budget, at least one entry is kept so
// an oversized single tool remains discoverable.
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

func (s *dynamicView) Require(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.require(names...)
}

// Discover adds or refreshes discovery entries. Each requested name
// must already be loaded (call EnsureLoaded first) so the pool measures
// real definitions; entries are then evicted LRU until the pool fits
// Policy.Discovery.
func (s *dynamicView) Discover(names ...string) DiscoverOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bumpEpoch()

	// Sizes come from the catalog's definitions, which is what the pool and the
	// per-round budget are measured against: a deployment that rewrote a
	// description changed the bytes on the wire, and measuring the tool's own
	// text here would make the two disagree. The snapshot is shared with
	// Definitions(), so both budget computations use the same figures.
	sizes := s.catalogSnapshotLocked().sizes

	reasons := make(map[string]string, len(names))
	changed := false
	rank := 0
	for _, name := range names {
		t, ok := s.catalog.Get(name)
		if !ok {
			reasons[name] = "unknown"
			continue
		}
		switch s.policy.exposureOf(name) {
		case ExposureDirect, ExposureDeferred:
		default:
			reasons[name] = "not_discoverable"
			continue
		}
		size, measured := sizes[name]
		if !measured {
			size = definitionBytes(t.Definition())
		}
		// names is ranked: rank 0 is the best hit, and the per-round
		// visible budget keeps the smaller ranks when it has to cut.
		s.st.touch(name, size, rank)
		rank++
		changed = true
	}

	var evicted []string
	if changed {
		evicted = s.evictForFitLocked()
	}
	// Report what the next Definitions call will send, not just what the
	// pool remembers: a freshly discovered name that loses the per-round
	// budget must not be announced as exposed.
	var visible map[string]struct{}
	if changed {
		visible = make(map[string]struct{})
		for _, def := range s.visibleDefinitionsLocked() {
			visible[def.Name] = struct{}{}
		}
	}
	results := make([]DiscoverResult, 0, len(names))
	for _, name := range names {
		result := DiscoverResult{Name: name}
		if reason := reasons[name]; reason != "" {
			// Unknown or undiscoverable names keep their own reason
			// even when an older pool entry still exists.
			result.Reason = reason
		} else if _, present := s.st.discovered[name]; !present {
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

// evictForFitLocked removes least-recently-used discovery entries until
// the pool fits MaxTools and MaxBytes. Between equally recent entries,
// Deferred tools are evicted before Direct ones; a single oversized
// entry is kept so discovery cannot dead-end on one big schema. Callers
// hold mu.
func (s *dynamicView) evictForFitLocked() []string {
	pool := s.policy.Discovery
	var evicted []string
	for {
		tools, total := s.st.discoveredTotals()
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
		for name, entry := range s.st.discovered {
			deferred := s.policy.exposureOf(name) == ExposureDeferred
			if first || evictsBefore(entry, deferred, victimEntry, victimDeferred) {
				victim = name
				victimEntry = entry
				victimDeferred = deferred
				first = false
			}
		}
		s.st.removeDiscovered(victim)
		evicted = append(evicted, victim)
	}
}

// evictsBefore reports whether a should be evicted before b: least
// recently used first; on recency ties Deferred before Direct; then
// oldest discovery sequence.
func evictsBefore(a discoveredEntry, aDeferred bool, b discoveredEntry, bDeferred bool) bool {
	if a.lastUse != b.lastUse {
		return a.lastUse < b.lastUse
	}
	if aDeferred != bDeferred {
		return aDeferred
	}
	return a.seq < b.seq
}

func (s *dynamicView) RecordCall(call message.ToolCall) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.policy.exposureOf(call.Name) {
	case ExposureDirect, ExposureDeferred:
	default:
		return
	}
	t, ok := s.catalog.Get(call.Name)
	if !ok {
		return
	}
	size, measured := s.catalogSnapshotLocked().sizes[call.Name]
	if !measured {
		size = definitionBytes(t.Definition())
	}
	s.st.touch(call.Name, size, 0)
	s.bumpEpoch()
	s.evictForFitLocked()
}

func (s *dynamicView) AdvanceRound() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bumpEpoch()

	alive := func(name string) bool {
		_, ok := s.catalog.Get(name)
		return ok
	}
	s.st.advanceTurn(s.policy.Discovery.IdleRounds, alive)
	// Definitions() only trims overflow in its read view; re-enforce the
	// pool cap on state here so entries dropped by the view are really
	// evicted even when no further Discover/RecordCall happens.
	s.evictForFitLocked()
}

func (s *dynamicView) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if s.policy.SearchWithLoad {
		if err := s.Load(ctx); err != nil {
			telemetry.WarnErr(ctx, "tool view: preload for search failed", err)
		}
	}
	return s.search(ctx, query, limit)
}

func (s *dynamicView) SearchWithLoad(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if err := s.Load(ctx); err != nil {
		telemetry.WarnErr(ctx, "tool view: preload for search failed", err)
	}
	return s.search(ctx, query, limit)
}

func (s *dynamicView) search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	s.mu.Lock()
	policy := s.policy
	s.mu.Unlock()

	// Every hit is loaded and takes a discovery-pool slot, so a
	// model-supplied limit cannot exceed the pool it would be loaded
	// into: a larger one only churns the pool with hits that can never
	// be exposed.
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if pool := policy.Discovery.MaxTools; pool > 0 && limit > pool {
		limit = pool
	}
	index, err := s.index(policy)
	if err != nil {
		return nil, err
	}
	return index.search(query, limit), nil
}

// index returns the BM25 index over the searchable definitions. Building it
// tokenizes the whole catalog, so the assembly keeps one and reuses it until the
// catalog's definitions change; a view without a cache rebuilds every call.
func (s *dynamicView) index(policy Policy) (*searchIndex, error) {
	build := func() []searchDoc {
		defs := s.catalog.Definitions()
		searchable := make([]message.ToolDefinition, 0, len(defs))
		for _, def := range defs {
			if policy.exposureOf(def.Name).searchable() {
				searchable = append(searchable, def)
			}
		}
		return searchDocs(searchable)
	}
	if s.searchCache == nil {
		return buildSearchIndex(build())
	}
	return s.searchCache.get(s.catalog, build)
}

func (s *dynamicView) Load(ctx context.Context) error {
	var errs []error
	for _, def := range s.catalog.Definitions() {
		if err := s.EnsureLoaded(ctx, def.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *dynamicView) EnsureLoaded(ctx context.Context, names ...string) error {
	var errs []error
	for _, name := range names {
		t, ok := s.catalog.Get(name)
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

// staticView is the non-dynamic view: every tool visible, no state.
type staticView struct {
	catalog tool.Catalog
}

// Epoch implements CatalogView: a static view never changes.
func (s *staticView) Epoch() uint64 { return 0 }

func (s *staticView) Get(name string) (tool.Tool, bool) {
	return s.catalog.Get(name)
}

func (s *staticView) Definitions() []message.ToolDefinition {
	return s.catalog.Definitions()
}

func (s *staticView) Require(...string) {}

func (s *staticView) Discover(...string) DiscoverOutcome {
	return DiscoverOutcome{}
}

func (s *staticView) RecordCall(message.ToolCall) {}
func (s *staticView) AdvanceRound()               {}
func (s *staticView) Load(context.Context) error  { return nil }
func (s *staticView) EnsureLoaded(context.Context, ...string) error {
	return nil
}

func (s *staticView) Search(context.Context, string, int) ([]SearchHit, error) {
	return nil, errdefs.NotAvailablef(
		"tool: dynamic injection is not enabled on this assembly")
}

func (s *staticView) SearchWithLoad(context.Context, string, int) ([]SearchHit, error) {
	return nil, errdefs.NotAvailablef(
		"tool: dynamic injection is not enabled on this assembly")
}

// CatalogViewFrom returns the per-run view carried by ctx's [tool.Scope]. The
// view travels as the scope's [tool.CallRecorder] — the contract seam middleware
// reads — so a middleware never has to depend on this package.
func CatalogViewFrom(ctx context.Context) (CatalogView, bool) {
	scope, ok := tool.ScopeFrom(ctx)
	if !ok || scope.Recorder == nil {
		return nil, false
	}
	view, ok := scope.Recorder.(CatalogView)
	return view, ok
}

// WithCatalogView attaches the per-run session to ctx.
func WithCatalogView(ctx context.Context, s CatalogView) context.Context {
	return tool.WithScope(ctx, tool.Scope{Recorder: s})
}

var (
	_ CatalogView = (*dynamicView)(nil)
	_ CatalogView = (*staticView)(nil)
)
