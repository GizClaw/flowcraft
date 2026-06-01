// Package materialize turns fused candidates back into grounded
// ContextItems by looking up their canonical fact in the temporal
// store and attaching embedded evidence (docs §9.4).
//
// Materialization is also the read-path's stale-fact filter: if a
// candidate's fact id is missing from the store (drift between
// retrieval doc and canonical ledger), the candidate is dropped and
// recorded in the trace. Recall does not auto-repair the drift; reconcile
// workers consume the diagnostic signal.
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
	store        port.TemporalStore
	observations port.ObservationStore
	links        port.LinkStore
	telemetry    port.TelemetryHook
}

var _ port.Materializer = (*FromStore)(nil)

// New constructs a FromStore. The telemetry hook is retained on the
// struct for forward compatibility but is intentionally NOT invoked
// here: drift signals (stale-fact / superseded-fact / scope-violation drops)
// flow through the read pipeline's CandidateMergeAndMaterializeDetail
// diagnostic that the stage emits to the same hook. Materializer stays a pure transform;
// emission is the stage's job.
func New(store port.TemporalStore, observations port.ObservationStore, links port.LinkStore, hook port.TelemetryHook) *FromStore {
	if hook == nil {
		hook = telemetry.NopHook{}
	}
	return &FromStore{store: store, observations: observations, links: links, telemetry: hook}
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
		if err := ctx.Err(); err != nil {
			return items, drops, err
		}
		switch c.Kind {
		case domain.GraphNodeAssertion:
			item, drop, ok, err := m.materializeAssertion(ctx, c)
			if err != nil {
				return items, drops, err
			}
			if !ok {
				drops = append(drops, drop)
				continue
			}
			items = append(items, item)
		case domain.GraphNodeObservation:
			item, drop, ok, err := m.materializeObservation(ctx, c)
			if err != nil {
				return items, drops, err
			}
			if !ok {
				drops = append(drops, drop)
				continue
			}
			items = append(items, item)
		case domain.GraphNodeLink:
			item, drop, ok, err := m.materializeLink(ctx, c)
			if err != nil {
				return items, drops, err
			}
			if !ok {
				drops = append(drops, drop)
				continue
			}
			items = append(items, item)
		default:
			drops = append(drops, diagnostic.CandidateDrop{
				Stage:   "candidate_materialize",
				Reason:  diagnostic.DropMaterializeErr,
				FactID:  c.ID,
				Source:  c.Source,
				Details: "candidate kind is required",
			})
		}
	}
	return items, drops, nil
}

func (m *FromStore) materializeAssertion(ctx context.Context, c domain.Candidate) (domain.ContextItem, diagnostic.CandidateDrop, bool, error) {
	fact, err := m.store.Get(ctx, c.Scope, c.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{}, false, err
		}
		if errors.Is(err, temporalstore.ErrNotFound) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{
				Stage:  "candidate_materialize",
				Reason: diagnostic.DropStaleFact,
				FactID: c.ID,
				Source: c.Source,
			}, false, nil
		}
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropMaterializeErr,
			FactID:  c.ID,
			Source:  c.Source,
			Details: err.Error(),
		}, false, nil
	}
	if fact.CorrectedBy != "" {
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:  "candidate_materialize",
			Reason: diagnostic.DropSuperseded,
			FactID: c.ID,
			Source: c.Source,
		}, false, nil
	}
	if reason, ok := violatesScope(c.Scope, fact.Scope); ok {
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropScopeViolation,
			FactID:  c.ID,
			Source:  c.Source,
			Details: reason,
		}, false, nil
	}
	return domain.ContextItem{
		Candidate: c,
		Ref:       c,
		Fact:      fact,
		Evidence:  selectCandidateEvidence(fact.EvidenceRefs, c.EvidenceIDs),
	}, diagnostic.CandidateDrop{}, true, nil
}

func (m *FromStore) materializeObservation(ctx context.Context, c domain.Candidate) (domain.ContextItem, diagnostic.CandidateDrop, bool, error) {
	if m.observations == nil {
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropMaterializeErr,
			FactID:  c.ID,
			Source:  c.Source,
			Details: "observation store is not configured",
		}, false, nil
	}
	obs, err := m.observations.Get(ctx, c.Scope, c.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{}, false, err
		}
		if errors.Is(err, port.ErrNotFound) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{
				Stage:  "candidate_materialize",
				Reason: diagnostic.DropStaleFact,
				FactID: c.ID,
				Source: c.Source,
			}, false, nil
		}
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropMaterializeErr,
			FactID:  c.ID,
			Source:  c.Source,
			Details: err.Error(),
		}, false, nil
	}
	return domain.ContextItem{
		Candidate:   c,
		Ref:         c,
		Observation: obs,
		Evidence:    []domain.EvidenceRef{evidenceRefFromObservation(obs)},
	}, diagnostic.CandidateDrop{}, true, nil
}

func (m *FromStore) materializeLink(ctx context.Context, c domain.Candidate) (domain.ContextItem, diagnostic.CandidateDrop, bool, error) {
	if m.links == nil {
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropMaterializeErr,
			FactID:  c.ID,
			Source:  c.Source,
			Details: "link store is not configured",
		}, false, nil
	}
	link, err := m.links.Get(ctx, c.Scope, c.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{}, false, err
		}
		if errors.Is(err, port.ErrNotFound) {
			return domain.ContextItem{}, diagnostic.CandidateDrop{
				Stage:  "candidate_materialize",
				Reason: diagnostic.DropStaleFact,
				FactID: c.ID,
				Source: c.Source,
			}, false, nil
		}
		return domain.ContextItem{}, diagnostic.CandidateDrop{
			Stage:   "candidate_materialize",
			Reason:  diagnostic.DropMaterializeErr,
			FactID:  c.ID,
			Source:  c.Source,
			Details: err.Error(),
		}, false, nil
	}
	return domain.ContextItem{
		Candidate: c,
		Ref:       c,
		Link:      link,
		Evidence:  append([]domain.EvidenceRef(nil), link.EvidenceRefs...),
	}, diagnostic.CandidateDrop{}, true, nil
}

func evidenceRefFromObservation(obs domain.Observation) domain.EvidenceRef {
	ts := obs.ObservedAt
	if ts.IsZero() {
		ts = obs.ReceivedAt
	}
	ref := domain.EvidenceRef{
		ID:            obs.ID,
		ObservationID: obs.ID,
		MessageID:     obs.MessageID,
		Role:          obs.Role,
		Text:          obs.Text,
		Timestamp:     ts,
	}
	if len(obs.Spans) > 0 {
		ref.SpanID = obs.Spans[0].ID
		if obs.Spans[0].Text != "" {
			ref.Text = obs.Spans[0].Text
		}
	}
	return ref
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
		if ref.ObservationID != "" {
			byID[ref.ObservationID] = ref
		}
		if ref.SpanID != "" {
			byID[ref.SpanID] = ref
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
		if key == "" {
			key = ref.SpanID
		}
		if key == "" {
			key = ref.ObservationID
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
