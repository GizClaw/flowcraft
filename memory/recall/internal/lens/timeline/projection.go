// Package timeline implements the optional timeline projection.
//
// It is a temporal view over event|state|plan facts, ordered by
// effective timestamp (ValidFrom ?? ObservedAt). It indexes any
// fact that is historical in the canonical sense (see
// domain.IsHistorical): not superseded by a successor and not
// soft-forgotten / TTL-expired. Past-ValidTo events REMAIN visible
// — a one-day event whose validity window has long closed must
// still answer "When did X happen?" queries; the timeline is a
// historical view, not an active-slot view.
package timeline

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
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

func (p *Projection) Consistency() port.Consistency { return port.Optional }

// AcceptsKind rejects KindEpisode. Episode facts are raw conversation
// captures; routing them through the timeline projection would
// pollute the event/state/plan timeline view with raw turn text.
func (p *Projection) AcceptsKind(k domain.FactKind) bool { return k != domain.KindEpisode }

// Project upserts timeline-eligible facts. Non-projectable facts
// (superseded, soft-closed, TTL-expired, or past their validity
// window) are evicted from the shard.
func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	if len(facts) == 0 {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" || !isTimelineKind(f.Kind) {
			continue
		}
		sh := p.shardLocked(f.Scope)
		delete(sh.byID, f.ID)
		for _, priorID := range f.Supersedes {
			delete(sh.byID, priorID)
		}
		if !domain.IsHistorical(f, now) {
			p.rebuildOrderLocked(sh)
			continue
		}
		e := entry{factID: f.ID, ts: domain.EffectiveTimestamp(f), kind: f.Kind}
		sh.byID[f.ID] = e
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

// ClearScope drops the entire scope shard for Memory.ForgetAll.
func (p *Projection) ClearScope(_ context.Context, scope domain.Scope) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return nil
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
