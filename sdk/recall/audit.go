package recall

import (
	"context"
	"fmt"
	"sort"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

// History returns the supersede chain for factID as a read-only view over
// TemporalStore. No journal adapter is required — append-only facts plus
// Supersedes / CorrectedBy links are the audit log. Soft-forgotten facts
// remain visible here even when default Recall hides them.
//
// Rollback is not a first-class API: callers may Save a new fact with
// Supersedes: []string{headID} to simulate rollback; a future RFC may add
// Memory.Rollback as sugar.
func (m *memory) History(ctx context.Context, scope Scope, factID string) ([]FactVersion, error) {
	if scope.RuntimeID == "" {
		return nil, errdefs.Validationf("recall.History: scope.runtime_id is required")
	}
	if factID == "" {
		return nil, errdefs.Validationf("recall.History: fact id is required")
	}
	facts, err := m.store.ListByID(ctx, scope, factID)
	if err != nil {
		return nil, fmt.Errorf("recall.History: %w", err)
	}
	out := make([]FactVersion, len(facts))
	for i, f := range facts {
		out[i] = domain.VersionFromFact(f)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ValidFrom.Before(out[j].ValidFrom)
	})
	return out, nil
}
