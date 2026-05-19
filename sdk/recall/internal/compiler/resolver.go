package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Resolution is the output of ConflictResolver. It separates two
// disjoint outcomes so Memory.Save can execute them transactionally:
//
//   - Facts: the facts that should be appended to the ledger
//     verbatim. Already includes any Supersedes pointers populated
//     by the resolver.
//   - Closes: previously-stored facts whose validity must be closed
//     after a successful Append. Each entry carries scope, fact id,
//     the ValidTo timestamp to write, and the new fact id that
//     supersedes it (becomes CorrectedBy).
//   - Drops: facts the resolver discarded (noop / dedupe), with a
//     structured reason for trace / telemetry.
type Resolution struct {
	Facts  []model.TemporalFact
	Closes []ValidityClose
	Drops  []DroppedFact
}

// ValidityClose instructs Memory.Save to close an existing fact's
// validity after the new facts have been appended.
type ValidityClose struct {
	Scope       model.Scope
	FactID      string
	ValidTo     time.Time
	CorrectedBy string
}

// ConflictResolver consults a read-only View to apply the §6.2
// idempotency rules.
//
// Implementations must remain free of write side-effects. The Save
// pipeline takes the returned Resolution and orchestrates Append +
// UpdateValidity atomically.
type ConflictResolver interface {
	ResolveConflicts(ctx context.Context, view View, facts []model.TemporalFact) (Resolution, error)
}

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
//   - The resolver never closes more than one prior fact per new
//     fact (the most-recent active one); explicit multi-fact
//     supersede goes through the upstream-supplied Supersedes
//     field which the resolver preserves.
type DefaultResolver struct {
	// Clock supplies the ValidTo timestamp written when a state /
	// preference fact closes an older revision. Defaults to
	// time.Now. Tests inject deterministic clocks.
	Clock func() time.Time
}

// NewResolver returns the default resolver wired with the system
// clock.
func NewResolver() *DefaultResolver { return &DefaultResolver{Clock: time.Now} }

// ResolveConflicts implements ConflictResolver.
func (r *DefaultResolver) ResolveConflicts(ctx context.Context, view View, facts []model.TemporalFact) (Resolution, error) {
	if view == nil {
		view = emptyView{}
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	var res Resolution

	for _, f := range facts {
		decision, err := r.classify(ctx, view, f)
		if err != nil {
			return Resolution{}, err
		}
		switch decision.action {
		case actionNoop:
			res.Drops = append(res.Drops, DroppedFact{Fact: f, Reason: decision.reason})
		case actionAppend:
			res.Facts = append(res.Facts, f)
		case actionSupersede:
			// Append new fact carrying Supersedes pointer; queue a
			// validity close on the prior fact. ValidTo uses the
			// new fact's ObservedAt when set, otherwise the
			// resolver clock — this matches §5.4 (state ValidTo =
			// time it was replaced).
			closeTime := f.ObservedAt
			if closeTime.IsZero() {
				closeTime = clock()
			}
			updated := f
			updated.Supersedes = mergeStrings(updated.Supersedes, []string{decision.priorID})
			res.Facts = append(res.Facts, updated)
			res.Closes = append(res.Closes, ValidityClose{
				Scope:       f.Scope,
				FactID:      decision.priorID,
				ValidTo:     closeTime,
				CorrectedBy: updated.ID,
			})
		}
	}
	return res, nil
}

type resolverAction int

const (
	actionAppend resolverAction = iota
	actionNoop
	actionSupersede
)

type resolverDecision struct {
	action  resolverAction
	reason  string
	priorID string
}

// classify applies the §6.2 idempotency rules to a single fact.
// View lookup errors propagate up so transient store failures fail
// the Save call cleanly instead of silently degrading to a fresh
// append that could duplicate or wrongly supersede.
func (r *DefaultResolver) classify(ctx context.Context, view View, f model.TemporalFact) (resolverDecision, error) {
	switch f.Kind {
	case model.KindEvent, model.KindPlan:
		// Events / plans are append-only by design. Even with a
		// matching merge_key, two separate event observations are
		// distinct ledger entries.
		return resolverDecision{action: actionAppend}, nil

	case model.KindRelation:
		// Relation merge_key already includes object (PR-2), so
		// the lookup below differentiates Alice/spouse/Bob from
		// Alice/spouse/Carol naturally. Same merge_key + identical
		// content is still a noop dedupe.
		return r.dedupeOrSupersede(ctx, view, f, false)

	case model.KindState, model.KindPreference:
		// Active state / preference with a changed value supersedes
		// the older revision.
		return r.dedupeOrSupersede(ctx, view, f, true)

	case model.KindNote:
		// Notes have no stable merge identity. Dedupe by
		// (source_message, content) when possible — i.e. if a
		// merge_key happens to exist (content-hash key from the
		// normalizer) and an existing fact in the same scope already
		// carries it, drop the new one.
		return r.dedupeOrSupersede(ctx, view, f, false)
	}
	return resolverDecision{action: actionAppend}, nil
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
func (r *DefaultResolver) dedupeOrSupersede(ctx context.Context, view View, f model.TemporalFact, supersedeOnChange bool) (resolverDecision, error) {
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
			action:  actionSupersede,
			reason:  "conflict:supersede",
			priorID: active.ID,
		}, nil
	}
	return resolverDecision{action: actionAppend}, nil
}

// mostRecentActive returns the youngest (latest ObservedAt) active
// fact visible to newAgent. Visibility follows the AgentID soft
// isolation rule (canSupersede) so agent-private state writes never
// supersede a different agent's private writes — and a fact with no
// AgentID ("shared") can only supersede other shared facts.
func mostRecentActive(facts []model.TemporalFact, newAgent string) *model.TemporalFact {
	var latest *model.TemporalFact
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
func sameContent(a, b model.TemporalFact) bool {
	return canonicalContent(a) == canonicalContent(b)
}

func canonicalContent(f model.TemporalFact) string {
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

// StoreView adapts a TemporalFactStore to the resolver View. Lives
// in this package so the compiler does not import the store
// package directly; callers (Memory.Save) construct it.
type StoreView struct {
	FindByMergeKeyFn func(ctx context.Context, scope model.Scope, mergeKey string) ([]model.TemporalFact, error)
	GetFn            func(ctx context.Context, scope model.Scope, factID string) (model.TemporalFact, error)
}

// FindByMergeKey implements View.
func (v StoreView) FindByMergeKey(ctx context.Context, scope model.Scope, mergeKey string) ([]model.TemporalFact, error) {
	if v.FindByMergeKeyFn == nil {
		return nil, nil
	}
	return v.FindByMergeKeyFn(ctx, scope, mergeKey)
}

// Get implements View.
func (v StoreView) Get(ctx context.Context, scope model.Scope, factID string) (model.TemporalFact, error) {
	if v.GetFn == nil {
		return model.TemporalFact{}, fmt.Errorf("recall compiler StoreView: Get not wired")
	}
	return v.GetFn(ctx, scope, factID)
}
