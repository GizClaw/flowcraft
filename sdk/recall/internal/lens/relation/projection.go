// Package relation implements the optional typed-relation projection.
//
// It indexes active relation facts (CorrectedBy empty, ValidTo open
// or in the future) for subject/predicate/object lookup (docs §8.3).
package relation

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Projection is an in-memory typed-relation index.
type Projection struct {
	mu     sync.RWMutex
	scopes map[scopeKey]*shard
}

type scopeKey struct {
	runtimeID string
	userID    string
}

type shard struct {
	// byTriple maps canonical (subject,predicate,object) -> fact id.
	byTriple map[string]string
	reverse  map[string]string // factID -> triple key
}

// New returns an empty relation projection.
func New() *Projection {
	return &Projection{scopes: make(map[scopeKey]*shard)}
}

func (p *Projection) Name() string { return "relation" }

func (p *Projection) Consistency() port.Consistency { return port.Optional }

// Project upserts active relation facts only.
func (p *Projection) Project(_ context.Context, facts []domain.TemporalFact) error {
	now := time.Now()
	if len(facts) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range facts {
		if f.ID == "" || f.Kind != domain.KindRelation {
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
		key := tripleKey(f.Subject, f.Predicate, f.Object, f.Scope.AgentID)
		if key == "" {
			continue
		}
		if prev, ok := sh.byTriple[key]; ok && prev != f.ID {
			delete(sh.reverse, prev)
		}
		sh.byTriple[key] = f.ID
		sh.reverse[f.ID] = key
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

// Lookup returns fact ids matching any supplied dimension. Empty
// subject/predicate/object means "don't filter on this dimension".
// All dimensions empty returns nil so callers cannot scan the scope.
func (p *Projection) Lookup(_ context.Context, scope domain.Scope, subject, predicate, object string) []string {
	subject = canonicalKeyPart(subject)
	predicate = canonicalKeyPart(predicate)
	object = canonicalKeyPart(object)
	if subject == "" && predicate == "" && object == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	sh, ok := p.scopes[keyOf(scope)]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for key, id := range sh.byTriple {
		sub, pred, obj := parseTripleKey(key)
		if subject != "" && sub != subject {
			continue
		}
		if predicate != "" && pred != predicate {
			continue
		}
		if object != "" && obj != object {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
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
			byTriple: make(map[string]string),
			reverse:  make(map[string]string),
		}
		p.scopes[k] = sh
	}
	return sh
}

func removeFactLocked(sh *shard, factID string) {
	key, ok := sh.reverse[factID]
	if !ok {
		return
	}
	if sh.byTriple[key] == factID {
		delete(sh.byTriple, key)
	}
	delete(sh.reverse, factID)
}

func keyOf(s domain.Scope) scopeKey {
	return scopeKey{runtimeID: s.RuntimeID, userID: s.UserID}
}

func tripleKey(subject, predicate, object, agentID string) string {
	subject = canonicalKeyPart(subject)
	predicate = canonicalKeyPart(predicate)
	object = canonicalKeyPart(object)
	if subject == "" && predicate == "" && object == "" {
		return ""
	}
	return subject + "\x00" + predicate + "\x00" + object + "\x00" + agentID
}

func canonicalKeyPart(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func parseTripleKey(key string) (subject, predicate, object string) {
	// split on null byte — max 3 parts
	var parts [3]string
	n := 0
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] != 0 {
			continue
		}
		if n < 3 {
			parts[n] = key[start:i]
			n++
		}
		start = i + 1
	}
	if n < 3 && start <= len(key) {
		parts[n] = key[start:]
		n++
	}
	if n > 0 {
		subject = parts[0]
	}
	if n > 1 {
		predicate = parts[1]
	}
	if n > 2 {
		object = parts[2]
	}
	return subject, predicate, object
}
