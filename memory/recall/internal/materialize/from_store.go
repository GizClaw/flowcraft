// Package materialize turns fused candidates back into grounded
// ContextItems by looking up their canonical fact in the temporal
// store and attaching embedded evidence (docs §9.4).
//
// Materialization is also the read-path's stale-fact filter: if a
// candidate's fact id is missing from the store (drift between
// retrieval doc and canonical ledger), the candidate is dropped and
// recorded in the trace. PR-3 does not auto-repair the drift —
// that's reconcile (Phase 5).
package materialize

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/internal/telemetry"
)

// FromStore materializes from a TemporalFactStore.
type FromStore struct {
	store     port.TemporalStore
	telemetry port.TelemetryHook
}

var _ port.Materializer = (*FromStore)(nil)

// New constructs a FromStore. The telemetry hook is retained on the
// struct for forward compatibility but is intentionally NOT invoked
// here: Phase E.3 collapsed the legacy DriftEvent emit path; drift
// signals (stale-fact / superseded-fact / scope-violation drops) now
// flow through the read pipeline's MaterializeDetail diagnostic that
// the stage emits to the same hook. Materializer therefore stays a
// pure transform — emission is the stage's job (docs §10.1: no
// auto-repair from Recall; reconcile workers subscribe to
// MaterializeDetail.Drops via TelemetryHook.OnStage).
func New(store port.TemporalStore, hook port.TelemetryHook) *FromStore {
	if hook == nil {
		hook = telemetry.NopHook{}
	}
	return &FromStore{store: store, telemetry: hook}
}

// Materialize loads each candidate's canonical fact. Drops fall in
// four buckets:
//
//   - DropStaleFact: store has no such id (retrieval doc drift).
//   - DropMaterializeErr: store returned a non-ErrNotFound error.
//   - DropSuperseded: fact.CorrectedBy != "" — revised state, do
//     not surface.
//   - DropScopeViolation: defense-in-depth scope check. The
//     candidate's query scope must hard-partition match the loaded
//     fact (runtime+user), and if the query scope carries an
//     AgentID the fact must be either agent-shared or written by
//     the same agent. A faulty / third-party CandidateSource that
//     bypasses scope filters is caught here so the read path stays
//     isolated per docs §16 invariants.
//
// Errors during materialization never abort the whole call: one
// bad candidate must not poison the rest of the recall.
func (m *FromStore) Materialize(ctx context.Context, candidates []domain.Candidate) ([]domain.ContextItem, []diagnostic.CandidateDrop, error) {
	var (
		items []domain.ContextItem
		drops []diagnostic.CandidateDrop
	)
	for _, c := range candidates {
		fact, err := m.store.Get(ctx, c.Scope, c.FactID)
		if err != nil {
			if errors.Is(err, temporalstore.ErrNotFound) {
				drops = append(drops, diagnostic.CandidateDrop{
					Stage:  "materialize",
					Reason: diagnostic.DropStaleFact,
					FactID: c.FactID,
					Source: c.Source,
				})
				continue
			}
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:   "materialize",
				Reason:  diagnostic.DropMaterializeErr,
				FactID:  c.FactID,
				Source:  c.Source,
				Details: err.Error(),
			})
			continue
		}
		if fact.CorrectedBy != "" {
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:  "materialize",
				Reason: diagnostic.DropSuperseded,
				FactID: c.FactID,
				Source: c.Source,
			})
			continue
		}
		if reason, ok := violatesScope(c.Scope, fact.Scope); ok {
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:   "materialize",
				Reason:  diagnostic.DropScopeViolation,
				FactID:  c.FactID,
				Source:  c.Source,
				Details: reason,
			})
			continue
		}
		items = append(items, domain.ContextItem{
			Candidate: c,
			Fact:      fact,
			Evidence:  selectCandidateEvidence(fact.EvidenceRefs, c.EvidenceIDs),
		})
	}
	return items, drops, nil
}

func selectCandidateEvidence(refs []domain.EvidenceRef, ids []string) []domain.EvidenceRef {
	if len(refs) == 0 {
		return nil
	}
	if len(ids) == 0 {
		return append([]domain.EvidenceRef(nil), refs...)
	}
	byID := make(map[string]domain.EvidenceRef, len(refs)*2)
	for _, ref := range refs {
		if ref.ID != "" {
			byID[ref.ID] = ref
		}
		if ref.MessageID != "" {
			byID[ref.MessageID] = ref
		}
	}
	out := make([]domain.EvidenceRef, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		ref, ok := byID[id]
		if !ok {
			continue
		}
		key := ref.ID
		if key == "" {
			key = ref.MessageID
		}
		if key != "" {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return append([]domain.EvidenceRef(nil), refs...)
	}
	return out
}

// violatesScope reports whether a loaded fact's canonical owner
// scope is incompatible with the query scope under the v2 isolation
// rules. Returns (reason, true) on violation, ("", false) on pass.
func violatesScope(query, owner domain.Scope) (string, bool) {
	if owner.RuntimeID != query.RuntimeID {
		return "runtime_id mismatch", true
	}
	if owner.UserID != query.UserID {
		return "user_id mismatch", true
	}
	if query.AgentID != "" && owner.AgentID != "" && owner.AgentID != query.AgentID {
		return "agent_id soft isolation", true
	}
	return "", false
}
