package recall

import (
	"context"
	"fmt"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// ErrCrossScope is returned when a revision operation targets a fact
// outside the supplied primary scope (federation sub-scopes are not
// writable via revision APIs).
var ErrCrossScope = errdefs.Forbidden(errdefs.New("recall: revision crosses scope boundary"))

// Fork appends a parallel revision of sourceFactID without closing
// the prior fact. Both facts remain active for recall. Retract an
// unwanted branch with Forget (Soft) once D.8 lands; today use
// Forget to remove a forked fact by id.
//
// Merge (same merge_key value change) uses Save with state/preference
// facts or explicit Supersedes hints — the resolver supersedes
// automatically.
func (m *memory) Fork(ctx context.Context, scope Scope, sourceFactID string, newFact TemporalFact) (SaveResult, error) {
	if sourceFactID == "" {
		return SaveResult{}, errdefs.Validationf("recall.Fork: source fact id is required")
	}
	src, err := m.store.Get(ctx, scope, sourceFactID)
	if err != nil {
		return SaveResult{}, err
	}
	if err := assertSameScope(scope, src.Scope); err != nil {
		return SaveResult{}, err
	}
	newFact.Scope = scope
	if newFact.MergeKey == "" && src.MergeKey != "" {
		newFact.MergeKey = src.MergeKey + ":fork"
	}
	domain.AttachRevision(&newFact, domain.Revision{
		Kind:         domain.RevisionFork,
		SourceFactID: sourceFactID,
	})
	return m.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{newFact}, ObservedAt: newFact.ObservedAt})
}

// Contest records a challenge against factID with supporting evidence.
// The target fact receives a small penalty; a contest satellite note
// is appended for auditability.
func (m *memory) Contest(ctx context.Context, scope Scope, factID string, evidence []EvidenceRef) (SaveResult, error) {
	if factID == "" {
		return SaveResult{}, errdefs.Validationf("recall.Contest: fact id is required")
	}
	target, err := m.store.Get(ctx, scope, factID)
	if err != nil {
		return SaveResult{}, err
	}
	if err := assertSameScope(scope, target.Scope); err != nil {
		return SaveResult{}, err
	}
	if err := evolutionPenalize(ctx, m, scope, factID, 0.1); err != nil {
		return SaveResult{}, err
	}
	contest := TemporalFact{
		Scope:        scope,
		Kind:         FactNote,
		Content:      fmt.Sprintf("contest of %s", factID),
		EvidenceRefs: evidence,
		ObservedAt:   time.Now(),
	}
	domain.AttachRevision(&contest, domain.Revision{
		Kind:         domain.RevisionContest,
		SourceFactID: factID,
	})
	return m.Save(ctx, scope, SaveRequest{Facts: []TemporalFact{contest}})
}

func assertSameScope(primary, factScope domain.Scope) error {
	if primary.RuntimeID != factScope.RuntimeID || primary.UserID != factScope.UserID {
		return ErrCrossScope
	}
	if primary.AgentID != "" && factScope.AgentID != "" && primary.AgentID != factScope.AgentID {
		return ErrCrossScope
	}
	return nil
}

func evolutionPenalize(ctx context.Context, m *memory, scope Scope, factID string, delta float64) error {
	return m.applyFeedback(ctx, scope, factID, 0, delta)
}
