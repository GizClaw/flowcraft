// Package graph implements the optional EntityGraph projection (docs §8.4).
//
// It is a derived exploration view over canonical facts: typed relation edges
// are traversable, while co-occurrence edges are kept diagnostic-only by
// default. It is not a truth layer.
package graph

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type edgeRef struct {
	factID    string
	predicate string
	agentID   string
}

type CooccurrenceDiagnostic struct {
	FactID string
	From   string
	To     string
}

type shard struct {
	// adj[from][to] lists edges leaving from->to (multiple facts allowed).
	adj          map[string]map[string][]edgeRef
	reverse      map[string][]directedEdge // factID -> traversable edges for Forget
	cooccurrence map[string][]directedEdge // factID -> diagnostic-only edges
}

// Projection is an in-memory entity graph per scope.
type Projection struct {
	mu     sync.RWMutex
	cfg    Config
	scopes map[scopeKey]*shard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

// New returns an empty graph projection with optional tuning.
func New(cfg ...Config) *Projection {
	var c Config
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return &Projection{cfg: c, scopes: make(map[scopeKey]*shard)}
}

func (p *Projection) Name() string { return "graph" }

func (p *Projection) Consistency() port.Consistency { return port.Optional }

// AcceptsKind rejects KindEpisode. Episode facts are raw conversation
// captures; routing them through the graph projection would create
// spurious nodes/edges from verbatim turn text.
func (p *Projection) AcceptsKind(k domain.FactKind) bool { return k != domain.KindEpisode }

// Project upserts edges derived from the supplied facts.
func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" {
			continue
		}
		sh := p.shardLocked(f.Scope)
		removeFactLocked(sh, f.ID)
		for _, priorID := range f.Supersedes {
			removeFactLocked(sh, priorID)
		}
		for _, e := range extractEdges(f, p.cfg, time.Now()) {
			addEdgeLocked(sh, e)
		}
		for _, e := range extractDiagnosticCooccurrenceEdges(f, p.cfg, time.Now()) {
			addCooccurrenceDiagnosticLocked(sh, e)
		}
	}
	return nil
}

func (p *Projection) Forget(_ context.Context, scope domain.Scope, factIDs []string) error {
	if len(factIDs) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	for _, id := range factIDs {
		removeFactLocked(sh, id)
	}
	return nil
}

// Rebuild exact-replaces the scope shard.
func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return p.Project(ctx, facts)
}

// ClearScope drops the entire scope shard for Memory.ForgetAll.
func (p *Projection) ClearScope(_ context.Context, scope domain.Scope) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return nil
}

// Traverse performs bounded BFS from seed nodes and returns fact ids
// discovered on edges, ordered by hop distance then fact id.
func (p *Projection) Traverse(_ context.Context, scope domain.Scope, seeds []string, maxHops, limit int) []string {
	if len(seeds) == 0 {
		return nil
	}
	maxHops = CapGraphHops(maxHops)
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}

	frontier := make([]string, 0, len(seeds))
	seenNode := make(map[string]struct{})
	for _, s := range seeds {
		n := canonicalNode(s)
		if n == "" || isCommonNoun(n) {
			continue
		}
		if _, dup := seenNode[n]; dup {
			continue
		}
		seenNode[n] = struct{}{}
		frontier = append(frontier, n)
	}
	if len(frontier) == 0 {
		return nil
	}

	type scored struct {
		hop    int
		factID string
	}
	var collected []scored
	seenFact := make(map[string]struct{})

	for hop := 1; hop <= maxHops && len(frontier) > 0; hop++ {
		sort.Strings(frontier)
		var next []string
		hopFacts := make(map[string]struct{})
		for _, node := range frontier {
			neighbors := sh.adj[node]
			tos := make([]string, 0, len(neighbors))
			for to := range neighbors {
				tos = append(tos, to)
			}
			sort.Strings(tos)
			for _, to := range tos {
				refs := append([]edgeRef(nil), neighbors[to]...)
				sort.Slice(refs, func(i, j int) bool {
					if refs[i].factID != refs[j].factID {
						return refs[i].factID < refs[j].factID
					}
					return refs[i].agentID < refs[j].agentID
				})
				reached := false
				for _, ref := range refs {
					if !edgeVisible(scope, ref.agentID) {
						continue
					}
					reached = true
					if _, dup := seenFact[ref.factID]; dup {
						continue
					}
					hopFacts[ref.factID] = struct{}{}
				}
				if !reached {
					continue
				}
				if _, ok := seenNode[to]; !ok {
					seenNode[to] = struct{}{}
					next = append(next, to)
				}
			}
		}
		hopFactIDs := make([]string, 0, len(hopFacts))
		for factID := range hopFacts {
			hopFactIDs = append(hopFactIDs, factID)
		}
		sort.Strings(hopFactIDs)
		for _, factID := range hopFactIDs {
			if _, dup := seenFact[factID]; dup {
				continue
			}
			seenFact[factID] = struct{}{}
			collected = append(collected, scored{hop: hop, factID: factID})
			if limit > 0 && len(collected) >= limit {
				break
			}
		}
		if limit > 0 && len(collected) >= limit {
			break
		}
		frontier = next
	}

	sort.Slice(collected, func(i, j int) bool {
		if collected[i].hop != collected[j].hop {
			return collected[i].hop < collected[j].hop
		}
		return collected[i].factID < collected[j].factID
	})

	out := make([]string, 0, len(collected))
	for _, s := range collected {
		out = append(out, s.factID)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (p *Projection) CooccurrenceDiagnostics(scope domain.Scope) []CooccurrenceDiagnostic {
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok || len(sh.cooccurrence) == 0 {
		return nil
	}
	var out []CooccurrenceDiagnostic
	for factID, edges := range sh.cooccurrence {
		for _, e := range edges {
			out = append(out, CooccurrenceDiagnostic{
				FactID: factID,
				From:   e.from,
				To:     e.to,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FactID != out[j].FactID {
			return out[i].FactID < out[j].FactID
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func (p *Projection) shardLocked(scope domain.Scope) *shard {
	k := keyOf(scope)
	sh, ok := p.scopes[k]
	if !ok {
		sh = &shard{
			adj:          make(map[string]map[string][]edgeRef),
			reverse:      make(map[string][]directedEdge),
			cooccurrence: make(map[string][]directedEdge),
		}
		p.scopes[k] = sh
	}
	return sh
}

func addEdgeLocked(sh *shard, e directedEdge) {
	m, ok := sh.adj[e.from]
	if !ok {
		m = make(map[string][]edgeRef)
		sh.adj[e.from] = m
	}
	m[e.to] = append(m[e.to], edgeRef{factID: e.factID, predicate: e.predicate, agentID: e.agentID})
	sh.reverse[e.factID] = append(sh.reverse[e.factID], e)
}

func addCooccurrenceDiagnosticLocked(sh *shard, e directedEdge) {
	if sh.cooccurrence == nil {
		sh.cooccurrence = make(map[string][]directedEdge)
	}
	sh.cooccurrence[e.factID] = append(sh.cooccurrence[e.factID], e)
}

func removeFactLocked(sh *shard, factID string) {
	edges, ok := sh.reverse[factID]
	if ok {
		for _, e := range edges {
			m := sh.adj[e.from]
			if m == nil {
				continue
			}
			list := m[e.to]
			filtered := list[:0]
			for _, ref := range list {
				if ref.factID != factID {
					filtered = append(filtered, ref)
				}
			}
			if len(filtered) == 0 {
				delete(m, e.to)
			} else {
				m[e.to] = filtered
			}
			if len(m) == 0 {
				delete(sh.adj, e.from)
			}
		}
		delete(sh.reverse, factID)
	}
	delete(sh.cooccurrence, factID)
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

// CapGraphHops clamps caller-supplied expansion depth to the default bounded
// expansion.
func CapGraphHops(hops int) int {
	if hops <= 0 {
		return DefaultMaxHops
	}
	if hops > DefaultMaxHops {
		return DefaultMaxHops
	}
	return hops
}

// edgeVisible applies AgentID soft isolation during traversal,
// matching materialize.violatesScope (docs §16).
func edgeVisible(query domain.Scope, edgeAgentID string) bool {
	if query.AgentID == "" {
		return true
	}
	if edgeAgentID == "" || edgeAgentID == query.AgentID {
		return true
	}
	return false
}
