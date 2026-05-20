// Package timeline implements the optional timeline projection.
//
// It is a temporal view over event|state|plan facts, ordered by
// effective timestamp (ValidFrom ?? ObservedAt). Unlike profile /
// relation this is NOT an active-slot view: a past event remains
// visible even when ValidTo is set — only CorrectedBy suppresses
// indexing (docs §8.3 / PR-6).
package timeline

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
)

// timelineKinds are the fact kinds indexed by this projection.
var timelineKinds = map[domain.FactKind]struct{}{
	domain.KindEvent: {},
	domain.KindState: {},
	domain.KindPlan:  {},
}

// Projection stores scope-local facts sorted by effective time.
type Projection struct {
	mu     sync.RWMutex
	scopes map[scopeKey]*shard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type entry struct {
	factID string
	ts     time.Time
	kind   domain.FactKind
}

type shard struct {
	ordered []entry
	byID    map[string]entry
}

// New returns an empty timeline projection.
func New() *Projection {
	return &Projection{scopes: make(map[scopeKey]*shard)}
}

func (p *Projection) Name() string { return "timeline" }

func (p *Projection) Consistency() port.Consistency { return projection.Optional }

// Project upserts timeline-eligible facts. Superseded facts are
// evicted; ValidTo in the past does NOT hide an event/plan.
func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" || !isTimelineKind(f.Kind) {
			continue
		}
		sh := p.shardLocked(f.Scope)
		delete(sh.byID, f.ID)
		if domain.IsSuperseded(f) {
			p.rebuildOrderLocked(sh)
			continue
		}
		e := entry{factID: f.ID, ts: domain.EffectiveTimestamp(f), kind: f.Kind}
		sh.byID[f.ID] = e
		for _, priorID := range f.Supersedes {
			delete(sh.byID, priorID)
		}
		p.rebuildOrderLocked(sh)
	}
	return nil
}

// Forget removes fact ids from the scope shard.
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
	p.rebuildOrderLocked(sh)
	return nil
}

// Rebuild exact-replaces the scope shard from the supplied snapshot.
// Memory passes IncludeSuperseded=true; this projection decides
// which facts belong in the temporal view.
func (p *Projection) Rebuild(ctx context.Context, scope domain.Scope, facts []domain.TemporalFact) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return p.Project(ctx, facts)
}

// Query returns fact ids in effective-time order matching the
// optional range and kind filter. Zero range means no time bounds.
func (p *Projection) Query(_ context.Context, scope domain.Scope, from, to time.Time, kinds []domain.FactKind, limit int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	kindSet := kindFilterSet(kinds)
	filterTime := !from.IsZero() || !to.IsZero()

	var out []string
	for _, e := range sh.ordered {
		if len(kindSet) > 0 {
			if _, ok := kindSet[e.kind]; !ok {
				continue
			}
		}
		if filterTime {
			if !from.IsZero() && e.ts.Before(from) {
				continue
			}
			if !to.IsZero() && e.ts.After(to) {
				continue
			}
		}
		out = append(out, e.factID)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (p *Projection) shardLocked(scope domain.Scope) *shard {
	k := keyOf(scope)
	sh, ok := p.scopes[k]
	if !ok {
		sh = &shard{byID: make(map[string]entry)}
		p.scopes[k] = sh
	}
	return sh
}

func (p *Projection) rebuildOrderLocked(sh *shard) {
	sh.ordered = sh.ordered[:0]
	for _, e := range sh.byID {
		sh.ordered = append(sh.ordered, e)
	}
	sort.SliceStable(sh.ordered, func(i, j int) bool {
		if sh.ordered[i].ts.Equal(sh.ordered[j].ts) {
			return sh.ordered[i].factID < sh.ordered[j].factID
		}
		return sh.ordered[i].ts.Before(sh.ordered[j].ts)
	})
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func isTimelineKind(k domain.FactKind) bool {
	_, ok := timelineKinds[k]
	return ok
}

func kindFilterSet(kinds []domain.FactKind) map[domain.FactKind]struct{} {
	if len(kinds) == 0 {
		return nil
	}
	out := make(map[domain.FactKind]struct{}, len(kinds))
	for _, k := range kinds {
		out[k] = struct{}{}
	}
	return out
}
