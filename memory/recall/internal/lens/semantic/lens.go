// Package semantic implements task-specific assertion projections used by
// reasoning-heavy recall paths.
package semantic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
)

type Lens struct {
	name     string
	weight   float64
	activate func(domain.QueryIntent) bool
}

func AssertionLens() Lens {
	return Lens{name: planner.SourceAssertion, weight: planner.WeightAssertion, activate: planner.ActivatesAssertion}
}

func (l Lens) Spec() planner.LensSpec {
	return planner.LensSpec{Name: l.name, Weight: l.weight, Activate: l.activate}
}

func (l Lens) Build(_ lens.Deps) (lens.Built, error) {
	p := NewProjection(l.name)
	return lens.Built{Projection: p, Source: NewSource(l.name, p)}, nil
}

type Projection struct {
	name   string
	mu     sync.RWMutex
	scopes map[scopeKey]*shard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type shard struct {
	byID map[string]semanticEntry
}

type semanticEntry struct {
	id        string
	scope     domain.Scope
	text      string
	terms     map[string]struct{}
	observed  time.Time
	validFrom *time.Time
}

func NewProjection(name string) *Projection {
	return &Projection{name: name, scopes: make(map[scopeKey]*shard)}
}

func (p *Projection) Name() string { return p.name }

func (p *Projection) Consistency() port.Consistency { return port.Optional }

func (p *Projection) AcceptsKind(k domain.FactKind) bool { return k != domain.KindEpisode }

func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" {
			continue
		}
		sh := p.shardLocked(f.Scope)
		delete(sh.byID, f.ID)
		for _, priorID := range f.Supersedes {
			delete(sh.byID, priorID)
		}
		if !domain.IsProjectable(f, now) || !projectionKindMatches(p.name, f) {
			continue
		}
		sh.byID[f.ID] = semanticEntry{
			id:        f.ID,
			scope:     f.Scope,
			text:      semanticSearchText(f),
			terms:     semanticSearchTerms(f),
			observed:  f.ObservedAt,
			validFrom: cloneTime(f.ValidFrom),
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
		delete(sh.byID, id)
	}
	return nil
}

func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return p.Project(ctx, facts)
}

func (p *Projection) ClearScope(_ context.Context, scope domain.Scope) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return nil
}

func (p *Projection) QuerySemantic(_ context.Context, scope domain.Scope, intent domain.QueryIntent, limit int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	out := selectSemanticEntries(sh.byID, scope, limit)
	sort.SliceStable(out, func(i, j int) bool {
		return semanticEntryBefore(out[i], out[j])
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	ids := make([]string, len(out))
	for i, entry := range out {
		ids[i] = entry.id
	}
	return ids
}

func selectSemanticEntries(entries map[string]semanticEntry, scope domain.Scope, limit int) []semanticEntry {
	if limit <= 0 {
		out := make([]semanticEntry, 0, len(entries))
		for _, entry := range entries {
			if entryMatchesAgent(entry.scope, scope) {
				out = append(out, entry)
			}
		}
		return out
	}
	out := make([]semanticEntry, 0, limit)
	for _, entry := range entries {
		if !entryMatchesAgent(entry.scope, scope) {
			continue
		}
		if len(out) < limit {
			out = append(out, entry)
			continue
		}
		worst := 0
		for i := 1; i < len(out); i++ {
			if semanticEntryBefore(out[worst], out[i]) {
				worst = i
			}
		}
		if semanticEntryBefore(entry, out[worst]) {
			out[worst] = entry
		}
	}
	return out
}

func semanticEntryBefore(a, b semanticEntry) bool {
	ta := entryTime(a)
	tb := entryTime(b)
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.id < b.id
}

func (p *Projection) shardLocked(scope domain.Scope) *shard {
	k := keyOf(scope)
	sh, ok := p.scopes[k]
	if !ok {
		sh = &shard{byID: make(map[string]semanticEntry)}
		p.scopes[k] = sh
	}
	return sh
}

type Source struct {
	name      string
	proj      *Projection
	BaseScore float64
}

func NewSource(name string, proj *Projection) *Source {
	return &Source{name: name, proj: proj, BaseScore: 1.1}
}

func (s *Source) Name() string { return s.name }

func (s *Source) Query(ctx context.Context, plan domain.QueryPlan) domain.SourceResult {
	budget := plan.SourceBudgets[s.name]
	if budget <= 0 {
		return domain.SourceResult{Source: s.name}
	}
	queryLimit := budget + 1
	started := time.Now()
	ids := s.proj.QuerySemantic(ctx, plan.Intent.Scope, plan.Intent, queryLimit)
	latency := time.Since(started)
	truncated := false
	if len(ids) > budget {
		ids = ids[:budget]
		truncated = true
	}
	candidates := make([]domain.Candidate, 0, len(ids))
	for i, id := range ids {
		candidates = append(candidates, domain.Candidate{
			Kind:   domain.GraphNodeAssertion,
			ID:     id,
			Scope:  plan.Intent.Scope,
			Source: s.name,
			Rank:   i + 1,
			Score:  s.BaseScore,
			DiscoverySignals: []domain.DiscoverySignal{{
				Source: s.name,
				Kind:   "source_variant",
				Value:  "structured_assertion",
				Score:  s.BaseScore,
			}},
			Metadata: map[string]any{"semantic_source": s.name},
		})
	}
	return domain.SourceResult{Source: s.name, Candidates: candidates, Truncated: truncated, Latency: latency}
}

func projectionKindMatches(name string, f domain.TemporalFact) bool {
	switch name {
	case planner.SourceAssertion:
		if f.Kind == domain.KindParameter {
			return true
		}
		return f.Subject != "" && f.Predicate != "" && f.Object != ""
	default:
		return false
	}
}

func semanticSearchText(f domain.TemporalFact) string {
	parts := []string{f.Content, f.Subject, f.Predicate, f.Object, f.Location, f.EvidenceText}
	parts = append(parts, parameterSearchParts(f)...)
	parts = append(parts, f.Entities...)
	parts = append(parts, f.Participants...)
	return words.CanonicalSurface(strings.Join(parts, " "))
}

func parameterSearchParts(f domain.TemporalFact) []string {
	if f.Kind != domain.KindParameter || len(f.Metadata) == 0 {
		return nil
	}
	keys := []string{
		domain.MetaParameterOwner,
		domain.MetaParameterNamespacePath,
		domain.MetaParameterNameSurface,
		domain.MetaParameterCanonicalName,
		domain.MetaParameterRawValue,
		domain.MetaParameterNormalizedValue,
		domain.MetaParameterUnit,
		domain.MetaParameterCondition,
		domain.MetaParameterConstraintOperator,
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if raw, ok := f.Metadata[key]; ok {
			out = append(out, strings.TrimSpace(fmt.Sprint(raw)))
		}
	}
	return out
}

func semanticSearchTerms(f domain.TemporalFact) map[string]struct{} {
	terms := words.SemanticQueryTerms(semanticSearchText(f))
	out := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		out[term] = struct{}{}
	}
	return out
}

func entryMatchesAgent(entryScope, queryScope domain.Scope) bool {
	return queryScope.AgentID == "" || entryScope.AgentID == "" || entryScope.AgentID == queryScope.AgentID
}

func entryTime(entry semanticEntry) time.Time {
	if entry.validFrom != nil && !entry.validFrom.IsZero() {
		return *entry.validFrom
	}
	return entry.observed
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
