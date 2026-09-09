package tool

import (
	"context"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Session is the per-run / per-conversation injection view over a
// [Catalog]. An Engine creates one at run start, feeds the model
// Definitions() each round, records calls, and advances turns.
//
// When the assembly was built without dynamic injection, the session
// is static: Definitions() shows every tool, and the stateful
// operations (Require/Discover/RecordCall/AdvanceTurn) are no-ops while
// Search returns NotAvailable.
type Session interface {
	Catalog

	// Require adds names to the RequiredByName set. Idempotent.
	Require(names ...string)
	// Discover adds names to the discovery pool (or refreshes their
	// recency). The pool is bounded by Policy.Discovery: entries are
	// evicted least-recently-used when the pool is over budget and
	// swept after idle rounds. A discovered tool must be loaded before
	// the next Definitions call, otherwise the model would see its
	// placeholder schema — tool_search loads each name before
	// discovering it.
	Discover(names ...string) DiscoverOutcome
	// RecordCall records that the model called name this round,
	// refreshing its discovery-pool recency. Always and Hidden tools
	// are skipped: the former are always visible, the latter must
	// never surface through use.
	RecordCall(call message.ToolCall)
	// AdvanceTurn moves to the next round, expiring discovery entries
	// that went idle and dropping names no longer in the catalog. Call
	// it once per inference round.
	AdvanceTurn()
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
}

// DiscoverResult reports one tool's discovery-pool outcome.
type DiscoverResult struct {
	Name    string `json:"name"`
	Exposed bool   `json:"exposed"`
	Reason  string `json:"reason,omitempty"` // "unknown" | "not_discoverable" | "over_budget"
}

// DiscoverOutcome summarizes one Discover call.
type DiscoverOutcome struct {
	// Results carries one entry per requested name, in input order.
	Results []DiscoverResult `json:"results"`
	// Evicted lists pool entries dropped by budget pressure (LRU)
	// while fitting the newly discovered names.
	Evicted []string `json:"evicted,omitempty"`
}

// dynamicSession is the stateful injection view. It reads tools from
// the shared registry (the execution surface) and applies Exposure /
// discovery pool / budget purely on the read side.
type dynamicSession struct {
	catalog Catalog
	policy  Policy

	mu sync.Mutex
	st sessionState
}

func (s *dynamicSession) Get(name string) (Tool, bool) {
	return s.catalog.Get(name)
}

func (s *dynamicSession) Definitions() []message.ToolDefinition {
	s.mu.Lock()
	policy := s.policy
	st := s.st.snapshot()
	s.mu.Unlock()

	all := s.catalog.Definitions()
	sizes := make(map[string]int64, len(all))
	cands := make([]candidate, 0, len(all))
	for _, def := range all {
		sizes[def.Name] = definitionBytes(def)
		cands = append(cands, candidate{
			name: def.Name,
			def:  def,
			exp:  policy.exposureOf(def.Name),
		})
	}
	st = fitDiscoveryPool(st, sizes, policy.Discovery)

	visible := visibleCandidates(cands, st, policy)
	out := make([]message.ToolDefinition, 0, len(visible))
	for _, cand := range visible {
		out = append(out, cand.def)
	}
	return out
}

// fitDiscoveryPool restricts the snapshot's discovered set to the pool
// budget, measured against the live definitions of the current round.
// Most-recently-used entries stay first; required tools are unaffected
// (they live in their own set). The read path never mutates state, so
// entries trimmed here are evicted for real on the next mutation or
// AdvanceTurn. Like the per-round budget, at least one entry is kept so
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

func (s *dynamicSession) Require(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.require(names...)
}

// Discover adds or refreshes discovery entries. Each requested name
// must already be loaded (call EnsureLoaded first) so the pool measures
// real definitions; entries are then evicted LRU until the pool fits
// Policy.Discovery.
func (s *dynamicSession) Discover(names ...string) DiscoverOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()

	reasons := make(map[string]string, len(names))
	changed := false
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
		s.st.touch(name, definitionBytes(t.Definition()))
		changed = true
	}

	var evicted []string
	if changed {
		evicted = s.evictForFitLocked()
	}
	results := make([]DiscoverResult, 0, len(names))
	for _, name := range names {
		_, present := s.st.discovered[name]
		result := DiscoverResult{Name: name, Exposed: present}
		if !present {
			if reason, ok := reasons[name]; ok {
				result.Reason = reason
			} else {
				result.Reason = "over_budget"
			}
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
func (s *dynamicSession) evictForFitLocked() []string {
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

func (s *dynamicSession) RecordCall(call message.ToolCall) {
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
	s.st.touch(call.Name, definitionBytes(t.Definition()))
	s.evictForFitLocked()
}

func (s *dynamicSession) AdvanceTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()

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

func (s *dynamicSession) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if s.policy.SearchWithLoad {
		if err := s.Load(ctx); err != nil {
			telemetry.WarnErr(ctx, "tool session: preload for search failed", err)
		}
	}
	return s.search(ctx, query, limit)
}

func (s *dynamicSession) SearchWithLoad(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if err := s.Load(ctx); err != nil {
		telemetry.WarnErr(ctx, "tool session: preload for search failed", err)
	}
	return s.search(ctx, query, limit)
}

func (s *dynamicSession) search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	s.mu.Lock()
	policy := s.policy
	s.mu.Unlock()

	defs := s.catalog.Definitions()
	docs := make([]searchDoc, 0, len(defs))
	for _, def := range defs {
		if !policy.exposureOf(def.Name).searchable() {
			continue
		}
		docs = append(docs, searchDoc{
			name: def.Name,
			text: def.Name + " " + def.Description,
		})
	}
	return bm25Search(docs, query, limit), nil
}

func (s *dynamicSession) Load(ctx context.Context) error {
	var first error
	for _, def := range s.catalog.Definitions() {
		if err := s.EnsureLoaded(ctx, def.Name); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *dynamicSession) EnsureLoaded(ctx context.Context, names ...string) error {
	var first error
	for _, name := range names {
		t, ok := s.catalog.Get(name)
		if !ok {
			continue
		}
		if lazy, ok := t.(interface{ EnsureLoaded(context.Context) error }); ok {
			if err := lazy.EnsureLoaded(ctx); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// staticSession is the non-dynamic view: every tool visible, no state.
type staticSession struct {
	catalog Catalog
}

func (s *staticSession) Get(name string) (Tool, bool) {
	return s.catalog.Get(name)
}

func (s *staticSession) Definitions() []message.ToolDefinition {
	return s.catalog.Definitions()
}

func (s *staticSession) Require(...string) {}

func (s *staticSession) Discover(...string) DiscoverOutcome {
	return DiscoverOutcome{}
}

func (s *staticSession) RecordCall(message.ToolCall) {}
func (s *staticSession) AdvanceTurn()                {}
func (s *staticSession) Load(context.Context) error  { return nil }
func (s *staticSession) EnsureLoaded(context.Context, ...string) error {
	return nil
}

func (s *staticSession) Search(context.Context, string, int) ([]SearchHit, error) {
	return nil, errdefs.NotAvailablef(
		"tool: dynamic injection is not enabled on this assembly")
}

func (s *staticSession) SearchWithLoad(context.Context, string, int) ([]SearchHit, error) {
	return nil, errdefs.NotAvailablef(
		"tool: dynamic injection is not enabled on this assembly")
}

// SessionFromContext returns the per-run session attached by the
// engine. Only tool_search and the session recorder read it; the
// session is explicit run state, not assembly wiring.
func SessionFromContext(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(Session)
	return s, ok
}

// WithSession attaches the per-run session to ctx.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, s)
}

type sessionContextKey struct{}

var (
	_ Session = (*dynamicSession)(nil)
	_ Session = (*staticSession)(nil)
)
