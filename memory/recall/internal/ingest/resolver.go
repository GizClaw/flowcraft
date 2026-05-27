package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DefaultResolver implements the canonical §6.2 idempotency table:
//
//	condition                                       | action
//	------------------------------------------------|------------------------
//	same source message + same normalized content   | noop / dedupe
//	same merge_key + active fact + changed value    | append + close old
//	event fact                                      | append-only
//	relation fact with different object             | separate facts
//	note without stable key                         | content-hash dedupe
//
// Notes:
//   - "Active" means CorrectedBy == "" (the canonical superseded
//     signal from PR-2 patch). A fact whose ValidTo is set by
//     intrinsic time (event end) is still active for merge
//     purposes.
//   - For events, relations, plans the resolver appends each fact
//     directly; only state/preference participate in
//     supersede-on-merge.
//   - Implicit (merge_key driven) supersede closes at most one
//     prior fact — the most-recent active one — to keep the
//     deterministic dedupe path 1:1. Explicit supersede via
//     Supersedes / MergeHints.Supersedes supports 1:N: every
//     listed prior fact is validated and closed atomically (D1
//     decision, 2026-05-21).
type DefaultResolver struct {
	// Clock supplies the ValidTo timestamp written when a state /
	// preference fact closes an older revision. Defaults to
	// time.Now. Tests inject deterministic clocks.
	Clock func() time.Time
}

// NewResolver returns the default resolver wired with the system
// clock.
func NewResolver() *DefaultResolver { return &DefaultResolver{Clock: time.Now} }

var (
	_ port.ConflictResolver = (*DefaultResolver)(nil)
	_ port.View             = (*batchView)(nil)
	_ port.View             = StoreView{}
)

// ResolveConflicts implements ConflictResolver.
func (r *DefaultResolver) ResolveConflicts(ctx context.Context, view port.View, facts []domain.TemporalFact) (domain.Resolution, error) {
	if view == nil {
		view = emptyView{}
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	var res domain.Resolution
	batch := newBatchView(view)

	for _, f := range facts {
		decision, err := r.classify(ctx, batch, f)
		if err != nil {
			return domain.Resolution{}, err
		}
		switch decision.action {
		case actionNoop:
			res.Drops = append(res.Drops, diagnostic.DroppedFact{Fact: f, Reason: decision.reason})
		case actionAppend:
			res.Facts = append(res.Facts, f)
			batch.trackAppend(f)
		case actionSupersede:
			// Append new fact carrying Supersedes pointer(s); queue
			// a validity close on every prior fact named in
			// decision.priorIDs. ValidTo uses the new fact's
			// ObservedAt when set, otherwise the resolver clock —
			// this matches §5.4 (state ValidTo = time it was
			// replaced). 1:N supersede (D1) emits N Closes from a
			// single successor fact; partial commit is impossible
			// because resolveExplicitSupersedes pre-validated every
			// target before reaching this branch.
			closeTime := f.ObservedAt
			if closeTime.IsZero() {
				closeTime = clock()
			}
			updated := f
			updated.Supersedes = mergeStrings(updated.Supersedes, decision.priorIDs)
			res.Facts = append(res.Facts, updated)
			for _, priorID := range decision.priorIDs {
				res.Closes = append(res.Closes, domain.ValidityClose{
					Scope:       f.Scope,
					FactID:      priorID,
					ValidTo:     closeTime,
					CorrectedBy: updated.ID,
				})
				batch.trackClose(priorID)
			}
			batch.trackAppend(updated)
		}
	}
	return res, nil
}

type batchView struct {
	base              port.View
	pendingByMergeKey map[string]domain.TemporalFact
	closing           map[string]struct{}
}

func newBatchView(base port.View) *batchView {
	return &batchView{
		base:              base,
		pendingByMergeKey: make(map[string]domain.TemporalFact),
		closing:           make(map[string]struct{}),
	}
}

func (v *batchView) trackAppend(f domain.TemporalFact) {
	if f.MergeKey == "" {
		return
	}
	v.pendingByMergeKey[f.MergeKey] = f
}

func (v *batchView) trackClose(factID string) {
	if factID == "" {
		return
	}
	v.closing[factID] = struct{}{}
}

func (v *batchView) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	var out []domain.TemporalFact
	if pending, ok := v.pendingByMergeKey[mergeKey]; ok {
		if _, closing := v.closing[pending.ID]; !closing {
			out = append(out, pending)
		}
	}
	if v.base == nil {
		return out, nil
	}
	base, err := v.base.FindByMergeKey(ctx, scope, mergeKey)
	if err != nil {
		return nil, err
	}
	for _, f := range base {
		if _, closing := v.closing[f.ID]; closing {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (v *batchView) Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	if _, closing := v.closing[factID]; closing {
		return domain.TemporalFact{}, errdefs.Conflictf("recall compiler batch view: fact %q is pending close", factID)
	}
	for _, f := range v.pendingByMergeKey {
		if f.ID == factID {
			return f, nil
		}
	}
	if v.base == nil {
		return domain.TemporalFact{}, errdefs.NotFoundf("recall compiler batch view: fact %q not found", factID)
	}
	return v.base.Get(ctx, scope, factID)
}

type resolverAction int

const (
	actionAppend resolverAction = iota
	actionNoop
	actionSupersede
)

type resolverDecision struct {
	action resolverAction
	reason string
	// priorIDs lists the prior facts this decision closes. For
	// actionAppend / actionNoop the slice is empty. For
	// actionSupersede it carries at least one ID; explicit
	// supersede (Supersedes / MergeHints.Supersedes) may carry N
	// IDs, while implicit merge_key supersede always carries exactly
	// one.
	priorIDs []string
	kind     domain.RevisionKind
}

// classify applies the §6.2 idempotency rules to a single fact.
// port.View lookup errors propagate up so transient store failures fail
// the Save call cleanly instead of silently degrading to a fresh
// append that could duplicate or wrongly supersede.
func (r *DefaultResolver) classify(ctx context.Context, view port.View, f domain.TemporalFact) (resolverDecision, error) {
	if rev, ok := domain.RevisionOf(f); ok {
		switch rev.Kind {
		case domain.RevisionFork, domain.RevisionContest:
			return resolverDecision{action: actionAppend, reason: "revision:" + string(rev.Kind), kind: rev.Kind}, nil
		}
	}
	if len(f.Supersedes) > 0 || len(f.MergeHints.Supersedes) > 0 {
		decision, err := r.resolveExplicitSupersedes(ctx, view, f)
		if err != nil {
			return resolverDecision{}, err
		}
		if decision.action != actionAppend || len(decision.priorIDs) > 0 {
			decision.kind = domain.RevisionSupersede
			return decision, nil
		}
	}
	switch f.Kind {
	case domain.KindEvent, domain.KindPlan:
		// Events / plans are append-only by design. Even with a
		// matching merge_key, two separate event observations are
		// distinct ledger entries.
		return resolverDecision{action: actionAppend}, nil

	case domain.KindRelation:
		// Relation merge_key already includes object (PR-2), so
		// the lookup below differentiates Alice/spouse/Bob from
		// Alice/spouse/Carol naturally. Same merge_key + identical
		// content is still a noop dedupe.
		return r.dedupeOrSupersede(ctx, view, f, false)

	case domain.KindState, domain.KindPreference, domain.KindProcedure:
		// Active state / preference / procedure with a changed value supersedes
		// the older revision.
		return r.dedupeOrSupersede(ctx, view, f, true)

	case domain.KindNote:
		// Notes have no stable merge identity. Dedupe by
		// (source_message, content) when possible — i.e. if a
		// merge_key happens to exist (content-hash key from the
		// normalizer) and an existing fact in the same scope already
		// carries it, drop the new one.
		return r.dedupeOrSupersede(ctx, view, f, false)
	}
	return resolverDecision{action: actionAppend}, nil
}

// resolveExplicitSupersedes closes facts named in Supersedes /
// MergeHints.Supersedes without requiring a merge_key collision.
//
// 1:N semantics: every listed prior must resolve via view.Get before
// the resolver returns actionSupersede. Any missing prior aborts with
// an errdefs.Validation error so the write pipeline refuses a partial
// commit. Targets are deduplicated (mergeStrings already does this)
// and returned in iteration order.
func (r *DefaultResolver) resolveExplicitSupersedes(ctx context.Context, view port.View, f domain.TemporalFact) (resolverDecision, error) {
	// mergeStrings dedupes across (a, b); pass both halves through
	// it so even a single-slice input like Supersedes=[a, a, b]
	// collapses to [a, b].
	targets := mergeStrings(f.Supersedes, f.MergeHints.Supersedes)
	targets = mergeStrings(nil, targets)
	if len(targets) == 0 {
		return resolverDecision{action: actionAppend}, nil
	}
	for _, priorID := range targets {
		if priorID == "" {
			return resolverDecision{}, errdefs.Validationf("resolver explicit supersede: empty prior id")
		}
		prior, err := view.Get(ctx, f.Scope, priorID)
		if err != nil {
			return resolverDecision{}, errdefs.Validationf("resolver explicit supersede: prior %q: %v", priorID, err)
		}
		if !canSupersede(f.Scope.AgentID, prior.Scope.AgentID) {
			return resolverDecision{}, errdefs.Validationf(
				"resolver explicit supersede: prior %q not visible to agent %q", priorID, f.Scope.AgentID)
		}
	}
	return resolverDecision{
		action:   actionSupersede,
		reason:   "revision:merge",
		priorIDs: targets,
		kind:     domain.RevisionSupersede,
	}, nil
}

// dedupeOrSupersede looks the fact up by merge_key. If an
// identical-content active fact exists, the new fact is a noop. If
// a non-identical active fact exists AND supersedeOnChange is true,
// the new fact supersedes the older one. Otherwise the new fact is
// a fresh append.
//
// AgentID isolation: only prior facts the new fact's agent is
// allowed to see (own + shared) participate in dedupe / supersede
// decisions. This keeps agent-private revisions from accidentally
// closing each other.
func (r *DefaultResolver) dedupeOrSupersede(ctx context.Context, view port.View, f domain.TemporalFact, supersedeOnChange bool) (resolverDecision, error) {
	if f.MergeKey == "" {
		return resolverDecision{action: actionAppend}, nil
	}
	prior, err := view.FindByMergeKey(ctx, f.Scope, f.MergeKey)
	if err != nil {
		return resolverDecision{}, fmt.Errorf("resolver lookup by merge_key %q: %w", f.MergeKey, err)
	}
	active := mostRecentActive(prior, f.Scope.AgentID)
	if active == nil {
		return resolverDecision{action: actionAppend}, nil
	}
	if sameContent(*active, f) {
		return resolverDecision{
			action: actionNoop,
			reason: "conflict:duplicate_content",
		}, nil
	}
	if supersedeOnChange {
		return resolverDecision{
			action:   actionSupersede,
			reason:   "conflict:supersede",
			priorIDs: []string{active.ID},
			kind:     domain.RevisionSupersede,
		}, nil
	}
	return resolverDecision{action: actionAppend}, nil
}

// mostRecentActive returns the youngest (latest ObservedAt) active
// fact visible to newAgent. Visibility follows the AgentID soft
// isolation rule (canSupersede) so agent-private state writes never
// supersede a different agent's private writes — and a fact with no
// AgentID ("shared") can only supersede other shared facts.
func mostRecentActive(facts []domain.TemporalFact, newAgent string) *domain.TemporalFact {
	var latest *domain.TemporalFact
	for i := range facts {
		f := &facts[i]
		if f.CorrectedBy != "" {
			continue
		}
		if !canSupersede(newAgent, f.Scope.AgentID) {
			continue
		}
		if latest == nil || f.ObservedAt.After(latest.ObservedAt) {
			latest = f
		}
	}
	return latest
}

// canSupersede encodes the agent visibility rule for write-path
// conflict resolution:
//
//   - A shared write (newAgent == "") may only supersede other
//     shared facts. It must never touch an agent-private fact.
//   - An agent write may supersede its own prior facts AND any
//     shared prior fact carrying the same merge_key (the shared
//     fact is older "common knowledge" the agent now refines).
//
// Together with the read-path materialize defense, this guarantees
// agent-a's writes cannot mutate agent-b's ledger view even though
// the canonical store partitions only on (runtime, user).
func canSupersede(newAgent, priorAgent string) bool {
	if newAgent == "" {
		return priorAgent == ""
	}
	return priorAgent == "" || priorAgent == newAgent
}

// sameContent compares two facts for canonical-content equality.
// The comparison purposefully ignores ID / ObservedAt / evidence /
// salience metadata so re-observing the same fact dedupes cleanly.
func sameContent(a, b domain.TemporalFact) bool {
	return canonicalContent(a) == canonicalContent(b)
}

func canonicalContent(f domain.TemporalFact) string {
	parts := []string{
		string(f.Kind),
		strings.ToLower(strings.TrimSpace(f.Subject)),
		strings.ToLower(strings.TrimSpace(f.Predicate)),
		strings.ToLower(strings.TrimSpace(f.Object)),
		strings.ToLower(strings.TrimSpace(f.Content)),
		strings.ToLower(strings.TrimSpace(f.Location)),
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// StoreView adapts a TemporalFactStore to the resolver port.View. Lives
// in this package so the compiler does not import the store
// package directly; callers (Memory.Save) construct it.
type StoreView struct {
	FindByMergeKeyFn func(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error)
	GetFn            func(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error)
}

// FindByMergeKey implements port.View.
func (v StoreView) FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error) {
	if v.FindByMergeKeyFn == nil {
		return nil, nil
	}
	return v.FindByMergeKeyFn(ctx, scope, mergeKey)
}

// Get implements port.View.
func (v StoreView) Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error) {
	if v.GetFn == nil {
		return domain.TemporalFact{}, errdefs.Internalf("recall compiler StoreView: Get not wired")
	}
	return v.GetFn(ctx, scope, factID)
}
