// Package profile implements the optional active-slot profile
// projection for state, preference, and relation facts (docs §8.3).
package profile

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Projection is an in-memory active-slot index keyed by subject.
type Projection struct {
	mu     sync.RWMutex
	scopes map[scopeKey]*shard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type shard struct {
	// bySubject maps subject -> slotKey -> factID
	bySubject map[string]map[string]string
	reverse   map[string]string // factID -> subject
	slotOf    map[string]string // factID -> slotKey
}

// New returns an empty profile projection.
func New() *Projection {
	return &Projection{scopes: make(map[scopeKey]*shard)}
}

func (p *Projection) Name() string { return "profile" }

func (p *Projection) Consistency() port.Consistency { return port.Optional }

// AcceptsKind rejects KindEpisode. Episode facts are raw conversation
// captures; routing them through the profile projection would pollute
// the canonical state / preference / relation views.
func (p *Projection) AcceptsKind(k domain.FactKind) bool { return k != domain.KindEpisode }

// Project upserts active state/preference/relation facts.
func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	now := time.Now()
	if len(facts) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" {
			continue
		}
		if !isProfileKind(f.Kind) {
			continue
		}
		sh := p.shardLocked(f.Scope)
		removeFactLocked(sh, f.ID)
		for _, priorID := range f.Supersedes {
			removeFactLocked(sh, priorID)
		}
		if !domain.IsProjectable(f, now) {
			continue
		}
		subject := canonicalKeyPart(f.Subject)
		if subject == "" {
			continue
		}
		slot := slotKey(f)
		if slot == "" {
			continue
		}
		m, ok := sh.bySubject[subject]
		if !ok {
			m = make(map[string]string)
			sh.bySubject[subject] = m
		}
		if prev, ok := m[slot]; ok && prev != f.ID {
			delete(sh.reverse, prev)
			delete(sh.slotOf, prev)
		}
		m[slot] = f.ID
		sh.reverse[f.ID] = subject
		sh.slotOf[f.ID] = slot
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

// ClearScope drops the entire scope shard. Backs Memory.ForgetAll (D.8 C9).
func (p *Projection) ClearScope(_ context.Context, scope domain.Scope) error {
	p.mu.Lock()
	delete(p.scopes, keyOf(scope))
	p.mu.Unlock()
	return nil
}

// Lookup returns all active fact ids for the given subject.
func (p *Projection) Lookup(_ context.Context, scope domain.Scope, subject string) []string {
	subject = canonicalKeyPart(subject)
	if subject == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	slots, ok := sh.bySubject[subject]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(slots))
	for _, id := range slots {
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
			bySubject: make(map[string]map[string]string),
			reverse:   make(map[string]string),
			slotOf:    make(map[string]string),
		}
		p.scopes[k] = sh
	}
	return sh
}

func removeFactLocked(sh *shard, factID string) {
	subject, ok := sh.reverse[factID]
	if !ok {
		return
	}
	slot := sh.slotOf[factID]
	if m, ok := sh.bySubject[subject]; ok {
		if m[slot] == factID {
			delete(m, slot)
			if len(m) == 0 {
				delete(sh.bySubject, subject)
			}
		}
	}
	delete(sh.reverse, factID)
	delete(sh.slotOf, factID)
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func isProfileKind(k domain.FactKind) bool {
	switch k {
	case domain.KindState, domain.KindPreference, domain.KindRelation:
		return true
	}
	return false
}

// slotKey encodes the active-slot identity for a profile fact.
// AgentID is part of the slot so private agent facts do not overwrite
// each other; empty AgentID is the shared slot namespace.
func slotKey(f domain.TemporalFact) string {
	subject := canonicalKeyPart(f.Subject)
	if subject == "" {
		return ""
	}
	agent := f.Scope.AgentID
	switch f.Kind {
	case domain.KindState, domain.KindPreference:
		predicate := canonicalKeyPart(f.Predicate)
		if predicate == "" {
			return subject + "\x00" + agent
		}
		return subject + "\x00" + predicate + "\x00" + agent
	case domain.KindRelation:
		return subject + "\x00" + canonicalKeyPart(f.Predicate) + "\x00" + canonicalKeyPart(f.Object) + "\x00" + agent
	}
	return ""
}

func canonicalKeyPart(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
