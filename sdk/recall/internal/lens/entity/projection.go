// Package entity implements the canonical entity mention projection.
//
// It is an inverted index from canonical entity mention -> fact ids,
// scoped per Scope. Recall reads it via an EntitySource; it is NOT a
// graph and not a truth layer (docs §8.2).
package entity

import (
	"context"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Projection is the in-memory entity mention projection. PR-2 ships
// the canonical implementation; durable backends arrive later without
// changing the boundary.
type Projection struct {
	mu     sync.RWMutex
	scopes map[scopeKey]*shard
}

// scopeKey is the entity projection partition key. AgentID is
// intentionally excluded so cross-agent recall within a single
// (runtime, user) still finds shared mentions — AgentID is a soft
// isolation dimension, not a partition.
type scopeKey struct {
	runtimeID string
	userID    string
}

type shard struct {
	// mentions maps canonical entity -> set of fact ids that
	// mention it. The set form lets Forget run in O(1) per fact.
	mentions map[string]map[string]struct{}
	// reverse maps fact id -> entities, used by Forget to know
	// which posting lists to update.
	reverse map[string][]string
}

// New returns an empty entity projection.
func New() *Projection {
	return &Projection{scopes: make(map[scopeKey]*shard)}
}

// Name implements port.Projection.
func (p *Projection) Name() string { return "entity" }

// Consistency reports Required: entity recall is part of the v2 read
// path, so the projection must be visible immediately after Save
// (docs §13 Phase 2).
func (p *Projection) Consistency() port.Consistency { return port.Required }

// Project upserts mentions for the supplied facts. Facts may belong
// to multiple scopes; they are routed per scope shard.
//
// Mirrors the retrieval projection's active-view rule: a fact with
// CorrectedBy != "" is silently dropped (and any existing mentions
// for the same id are evicted). Under normal Save flow this is a
// no-op; rebuild and any future bulk-replay path benefit from the
// projection self-cleaning.
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
		// Replace any existing mentions for this fact id so re-projects
		// (rebuild / supersede) do not leak old entities. The removal
		// also covers the superseded-skip case below: a fact that was
		// previously active but is now superseded gets evicted here.
		removeFactLocked(sh, f.ID)
		for _, priorID := range f.Supersedes {
			removeFactLocked(sh, priorID)
		}
		if f.CorrectedBy != "" {
			continue
		}
		ents := collectEntities(f)
		if len(ents) == 0 {
			continue
		}
		sh.reverse[f.ID] = ents
		for _, e := range ents {
			set, ok := sh.mentions[e]
			if !ok {
				set = make(map[string]struct{})
				sh.mentions[e] = set
			}
			set[f.ID] = struct{}{}
		}
	}
	return nil
}

// Forget removes the supplied fact ids from the scope shard.
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

// Rebuild drops the scope shard and re-projects facts from scratch.
// This is the canonical way to recover from drift (docs §8).
func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return p.Project(ctx, facts)
}

// Snapshot is one canonical entity the projection currently knows
// about in a scope, plus any aliases collected through prior
// projects. The compiler's Structurizer uses these as a write-time
// canonicalisation hint so fresh mentions fold into the canonical
// form instead of fragmenting the graph.
type Snapshot struct {
	Canonical string
	Aliases   []string
}

// Snapshot returns the canonical entities currently indexed for
// scope, sorted by descending mention count (ties broken
// alphabetically). The mention count is a soft "salience" signal:
// the more facts already mention an entity, the more likely a fresh
// mention of the same surface form refers to it. Returns nil when
// the scope has no entries.
//
// This is the read-side companion to Project; it is intentionally
// cheap (one map walk) so memory.Save can call it on every write
// without bloating the critical path.
func (p *Projection) Snapshot(scope domain.Scope) []Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok || len(sh.mentions) == 0 {
		return nil
	}
	out := make([]Snapshot, 0, len(sh.mentions))
	for canonical := range sh.mentions {
		if canonical == "" {
			continue
		}
		// Aliases stay nil for now: the v2 projection
		// canonicalises before indexing, so the canonical
		// form already encodes case / whitespace variation.
		// Adding alias surface forms is a follow-up once the
		// resolver pipes them through.
		out = append(out, Snapshot{Canonical: canonical})
	}
	// Stable order: canonical name. The Structurizer iterates
	// snapshots linearly so a deterministic order keeps test
	// goldens reproducible.
	sort.Slice(out, func(i, j int) bool { return out[i].Canonical < out[j].Canonical })
	return out
}

// Lookup returns the fact ids that mention any of the supplied
// entities within scope. Used by the future entity source; exported
// for tests in PR-2.
func (p *Projection) Lookup(_ context.Context, scope domain.Scope, entities []string) []string {
	if len(entities) == 0 {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, e := range entities {
		canon := canonicalEntity(e)
		if canon == "" {
			continue
		}
		for id := range sh.mentions[canon] {
			seen[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (p *Projection) shardLocked(scope domain.Scope) *shard {
	k := keyOf(scope)
	sh, ok := p.scopes[k]
	if !ok {
		sh = &shard{
			mentions: make(map[string]map[string]struct{}),
			reverse:  make(map[string][]string),
		}
		p.scopes[k] = sh
	}
	return sh
}

func removeFactLocked(sh *shard, factID string) {
	ents, ok := sh.reverse[factID]
	if !ok {
		return
	}
	for _, e := range ents {
		if set, ok := sh.mentions[e]; ok {
			delete(set, factID)
			if len(set) == 0 {
				delete(sh.mentions, e)
			}
		}
	}
	delete(sh.reverse, factID)
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func collectEntities(f domain.TemporalFact) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		canon := canonicalEntity(s)
		if canon == "" {
			return
		}
		if _, ok := seen[canon]; ok {
			return
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	for _, e := range f.Entities {
		add(e)
	}
	for _, p := range f.Participants {
		add(p)
	}
	add(f.Subject)
	add(f.Object)
	return out
}

// canonicalEntity lower-cases and trims whitespace. The compiler
// normalizer already does this for Entities; we re-apply here so
// Lookup works for ad-hoc callers (tests, future planners) that
// pass raw user input.
func canonicalEntity(s string) string {
	// reuse small ASCII path; non-ASCII letters pass through to the
	// lower-casing form. Entities are typically ASCII tokens after
	// the normalizer; non-ASCII is supported via strings.ToLower at
	// the canonicalizer boundary in the compiler.
	lower := make([]byte, 0, len(s))
	prevSpace := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if !prevSpace {
				lower = append(lower, ' ')
				prevSpace = true
			}
		case c >= 'A' && c <= 'Z':
			lower = append(lower, c+('a'-'A'))
			prevSpace = false
		default:
			lower = append(lower, c)
			prevSpace = false
		}
	}
	// trim trailing space
	for len(lower) > 0 && lower[len(lower)-1] == ' ' {
		lower = lower[:len(lower)-1]
	}
	return string(lower)
}
