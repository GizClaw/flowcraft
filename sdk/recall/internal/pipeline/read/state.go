// Package read owns the read-flow pipeline State and Runner.
// Stages (intent / plan / federation_fanout / source_fanout / fuse
// / materialize / federation_merge / trust_filter / rank /
// build_hits / evolution_after_recall) land in Phase B.3 / D.2 /
// D.5; this package owns the State schema so each stage stays
// narrow.
//
// Federation note: the State is federation-ready from day 1 on
// purpose. ReadState's MergedItems / SubScopeStates layout is the
// Phase D.5 target shape, not a flat single-scope shortcut. Non-
// federation runs use len(SubScopeStates)==1 and the federation_
// merge stage's Conditional.Skip path; the rest of the read
// pipeline does not change shape between simple and federated
// recalls. This avoids the invasive ReadState refactor noted in
// the Phase B.3 risk register (recall-v2-migration-plan.md §3.B.3).
package read

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
)

// ReadState is the per-call workspace threaded through every Stage
// of the read pipeline. Each stage reads inputs from previous
// fields and populates its own output slot; no Stage is allowed to
// route data through context.Value.
//
// Field ownership by Phase B.3 / D.2 / D.5 stage:
//
//	intent                  → Intent
//	plan                    → Plan
//	federation_fanout (D.5) → SubScopeStates (1 entry in the
//	                         single-scope fast path; per
//	                         sub-scope inside source_fanout +
//	                         fuse + materialize)
//	source_fanout           → SubScopeStates[i].SourceResults
//	fuse                    → SubScopeStates[i].Fused
//	materialize             → SubScopeStates[i].Materialized
//	federation_merge (D.5)  → MergedItems  (Conditional.Skip on
//	                         len(SubScopeStates)==1)
//	trust_filter (D.2)      → AfterTrust
//	rank                    → Ranked
//	build_hits              → Hits
//	evolution_after_recall  → no state mutation; best-effort
type ReadState struct {
	// Inputs — populated by the runner before Run begins.

	// Scope is the primary recall scope; its Federation field (set
	// in Phase D.5) drives sub-scope fan-out.
	Scope domain.Scope

	// Query is the caller's recall request after the public
	// facade copies it. Stages never reach back to the facade.
	Query domain.Query

	// Now is captured once at Pipeline entry so every stage
	// observes the same wall clock (deterministic ValidFrom /
	// ValidTo filters across slow stages).
	Now time.Time

	// StartedAt is the wall clock at Pipeline.Run entry, used by
	// the runner to fill RecallTrace.TotalLatency. Distinct from
	// Now (which a future stage may overwrite for deterministic
	// time-range queries).
	StartedAt time.Time

	// Stage outputs — populated in order, each stage owns
	// exactly one field group below.

	// Intent is the structurized query produced by the intent
	// stage. Plan stage consumes it; later stages read it via
	// Plan.Intent.
	Intent *domain.QueryIntent

	// Plan is the top-level query plan. In federated reads the
	// federation_fanout stage may rebuild a per-sub-scope plan
	// (different KnownEntities → different SourceOrder); Plan
	// stays the primary-scope baseline so dashboards can
	// attribute "why was this lens activated" without scanning
	// every sub-scope.
	Plan *domain.QueryPlan

	// SubScopeStates holds per-sub-scope candidate / fused /
	// materialized state. len==1 is the default non-federation
	// shape; len>=1 with len(Scope.Federation)>0 is the federated
	// path (Phase D.5). source_fanout / fuse / materialize run
	// once per entry; federation_merge folds them into
	// MergedItems.
	SubScopeStates []SubScopeState

	// MergedItems is the materialized item set after sub-scope
	// merging (Phase D.5 federation_merge). On the single-scope
	// fast path the federation_merge stage's Conditional.Skip
	// path leaves MergedItems empty; the runner promotes
	// SubScopeStates[0].Materialized into MergedItems before
	// downstream stages run so trust_filter / rank see a uniform
	// slice regardless of federation.
	MergedItems []domain.ContextItem

	// AfterTrust is the trust_filter (Phase D.2) output. Stages
	// downstream of trust_filter read this; the runner populates
	// it from MergedItems when trust_filter is disabled.
	AfterTrust []domain.ContextItem

	// Ranked is the rank stage output (and the input to
	// build_hits). Distinct from AfterTrust so explain traces
	// can attribute rank's reordering separately.
	Ranked []domain.ContextItem

	// Hits is the build_hits stage output and the value the
	// facade hands back to the caller via Memory.Recall.
	Hits []domain.Hit

	// RerankErr captures a non-fatal reranker failure for the
	// legacy telemetry bridge (build_hits stage).
	RerankErr error

	// Reranked is the hit count after a successful rerank pass.
	Reranked int

	// EvolutionErr captures a non-fatal AfterRecall failure for the
	// legacy telemetry bridge.
	EvolutionErr error

	// Trace is the in-flight RecallTrace. Pipeline.AppendTrace
	// pushes every emitted StageDiagnostic into Trace.Stages.
	// nil is permitted (Recall vs RecallExplain): the runner
	// allocates one only when explain is requested.
	Trace *domain.RecallTrace
}

// SubScopeState is one sub-scope's slice of the federated read
// pipeline. Phase D.5's federation_fanout creates one per entry of
// scope.EffectiveFederation(); the non-federation path constructs
// a single SubScopeState wrapping Scope.
//
// FastPath records the "single sub-scope, no merge needed" shortcut
// so federation_merge's Conditional.Skip can compute its decision
// off the State alone (no separate "is federated" flag) and
// dashboards can tell apart a Skip caused by single-scope vs a
// Skip caused by future policy.
type SubScopeState struct {
	// Scope is this sub-scope's full qualifier. Stages that hit
	// the temporal store / projections use this; the top-level
	// ReadState.Scope is only the primary.
	Scope domain.Scope

	// Plan is the per-sub-scope plan. Federation_fanout reruns
	// the planner per sub-scope because KnownEntities differs
	// between scopes (recall-v2-migration-plan.md §3.D.5 risk
	// note); non-federation runs share the primary Plan.
	Plan *domain.QueryPlan

	// SourceResults captures each registered Source's
	// contribution. source_fanout populates it; fuse consumes it.
	SourceResults []domain.SourceResult

	// Fused is the post-fusion candidate set (per sub-scope).
	// materialize consumes it.
	Fused []domain.Candidate

	// FusionDrops records candidates the fuser discarded
	// (duplicate-fact-id / per-source-cap / total-cap). Stored
	// per-sub-scope so the eventual federation_merge can roll
	// per-scope drift attribution into one cross-scope view.
	FusionDrops []diagnostic.CandidateDrop

	// Materialized is the per-sub-scope materialized item set.
	// federation_merge dedups by FactID across sub-scopes; on
	// the single-scope fast path the runner promotes this slice
	// into ReadState.MergedItems directly.
	Materialized []domain.ContextItem

	// MaterializeDrops records candidates the materializer
	// discarded (stale fact / superseded / scope violation).
	// Kept per-sub-scope for the same attribution reason as
	// FusionDrops.
	MaterializeDrops []diagnostic.CandidateDrop

	// FastPath is true when this sub-scope is the only entry in
	// SubScopeStates (len==1 single-scope read). federation_merge
	// reads it to short-circuit; rank / build_hits ignore it.
	FastPath bool
}

// EnsureTrace allocates the RecallTrace if explain output was
// requested but the caller did not pre-populate it.
func (s *ReadState) EnsureTrace() *domain.RecallTrace {
	if s.Trace == nil {
		s.Trace = &domain.RecallTrace{}
	}
	return s.Trace
}

// AppendStage is the TraceAppender the runner registers with the
// pipeline framework. It is a no-op when Trace is nil so callers
// requesting only the Hits slice (no explain) pay zero allocation.
func (s *ReadState) AppendStage(d diagnostic.StageDiagnostic) {
	if s.Trace == nil {
		return
	}
	s.Trace.Stages = append(s.Trace.Stages, d)
}

// PrimarySubScope returns the SubScopeState that corresponds to
// the primary recall scope, or nil when SubScopeStates is empty.
// federation_fanout guarantees the primary scope is at index 0;
// downstream stages use this helper to access "the canonical
// sub-scope" without re-implementing the lookup.
func (s *ReadState) PrimarySubScope() *SubScopeState {
	if s == nil || len(s.SubScopeStates) == 0 {
		return nil
	}
	return &s.SubScopeStates[0]
}
