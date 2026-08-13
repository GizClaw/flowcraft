package tool

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Session is the per-run / per-conversation injection view over a
// [Catalog]. An Engine creates one at run start, feeds the model
// Definitions() each round, records calls, and advances turns.
//
// When the assembly was built without dynamic injection, the session
// is static: Definitions() shows every tool, and the stateful
// operations (Select/Require/RecordCall/AdvanceTurn) are no-ops while
// Search returns NotAvailable.
type Session interface {
	Catalog

	// Require adds names to the RequiredByName set. Idempotent.
	Require(names ...string)
	// Select marks names as selected for the policy's retention
	// rounds. Selection carries an implicit load contract: a selected
	// tool must be loaded before the next Definitions call, otherwise
	// the model would see its placeholder schema. tool_search enforces
	// this by loading each name before selecting.
	Select(names ...string)
	// RecordCall records that the model called name this round,
	// refreshing its Selected and UsedRecently state.
	RecordCall(call message.ToolCall)
	// AdvanceTurn moves to the next round, expiring Selected and
	// UsedRecently entries. Call it once per inference round.
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

// dynamicSession is the stateful injection view. It reads tools from
// the shared registry (the execution surface) and applies Exposure /
// budget / selection purely on the read side.
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
	cands := make([]candidate, 0, len(all))
	for _, def := range all {
		cands = append(cands, candidate{
			name: def.Name,
			def:  def,
			exp:  policy.exposureOf(def.Name),
		})
	}
	visible := visibleCandidates(cands, st, policy)
	out := make([]message.ToolDefinition, 0, len(visible))
	for _, cand := range visible {
		out = append(out, cand.def)
	}
	return out
}

func (s *dynamicSession) Require(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.require(names...)
}

func (s *dynamicSession) Select(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.selectNames(names, s.policy.SelectedRetention)
}

func (s *dynamicSession) RecordCall(call message.ToolCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.recordCall(call.Name, s.policy.SelectedRetention)
}

func (s *dynamicSession) AdvanceTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.advanceTurn(s.policy.RecentWindow)
}

func (s *dynamicSession) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if s.policy.SearchWithLoad {
		_ = s.Load(ctx)
	}
	return s.search(ctx, query, limit)
}

func (s *dynamicSession) SearchWithLoad(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	_ = s.Load(ctx)
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

func (s *staticSession) Require(...string)           {}
func (s *staticSession) Select(...string)            {}
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
// engine. Only tool_search and RecordCalls read it; the session is
// explicit run state, not assembly wiring.
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
