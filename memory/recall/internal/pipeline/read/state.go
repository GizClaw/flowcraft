// Package read owns the read-flow pipeline State and Runner.
// Stages (query_understand / plan / candidate_fanout /
// candidate_merge_and_materialize / candidate_expansion / policy_filter /
// rank / context_pack / build_grounded_hits / evolution_after_recall) land
// here; this package owns the State schema so each stage stays narrow.
//
// Federation note: ReadState's MergedItems / SubScopeStates layout is the
// canonical shape. Non-federation runs use len(SubScopeStates)==1; candidate
// merging keeps the rest of the read pipeline identical between simple and
// federated recalls.
package read

import (
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// ReadState is the per-call workspace threaded through every Stage
// of the read pipeline. Each stage reads inputs from previous
// fields and populates its own output slot; no Stage is allowed to
// route data through context.Value.
//
// Field ownership by read stage:
//
//	query_understand                → Intent
//	plan                            → Plan
//	candidate_fanout                → SubScopeStates[i].SourceResults
//	candidate_merge_and_materialize → SubScopeStates[i].Candidates /
//	                                   SubScopeStates[i].Materialized /
//	                                   MergedItems
//	candidate_expansion             → MergedItems
//	policy_filter                   → AfterTrust
//	rank                            → Ranked
//	context_pack                    → Hits
//	build_grounded_hits             → Hits
//	evolution_after_recall          → no state mutation; best-effort
type ReadState struct {
	// Inputs — populated by the runner before Run begins.

	// Scope is the primary recall scope; its Federation field drives
	// sub-scope fanout.
	Scope domain.Scope

	// Query is the caller's recall request after the public
	// facade copies it. Stages never reach back to the facade.
	Query domain.Query

	// Now is captured once at Pipeline entry so every stage
	// observes the same wall clock (deterministic ValidFrom /
	// ValidTo filters across slow stages).
	Now time.Time

	// StartedAt is the wall clock at Pipeline.Run entry. Each stage's
	// diagnostic carries its own Duration; total latency is the sum across
	// trace.Stages.
	StartedAt time.Time

	// Stage outputs — populated in order, each stage owns
	// exactly one field group below.

	// Intent is the structurized query produced by the query_understand
	// stage. Plan stage consumes it; later stages read it via
	// Plan.Intent.
	Intent *domain.QueryIntent

	// Plan is the top-level query plan. In federated reads the
	// candidate_fanout stage may rebuild a per-sub-scope plan
	// (different KnownEntities → different SourceOrder); Plan
	// stays the primary-scope baseline so dashboards can
	// attribute "why was this lens activated" without scanning
	// every sub-scope.
	Plan *domain.QueryPlan

	// SubScopeStates holds per-sub-scope candidate / fused /
	// materialized state. len==1 is the default non-federation
	// shape; len>=1 with len(Scope.Federation)>0 is the federated
	// path. candidate_fanout and candidate_merge_and_materialize run
	// once per entry, then fold results into MergedItems.
	SubScopeStates []SubScopeState

	// MergedItems is the materialized item set after sub-scope
	// merging and candidate expansion. Downstream stages read this
	// uniform slice regardless of federation.
	MergedItems []domain.ContextItem

	// AfterTrust is the policy_filter output. Stages downstream of
	// policy_filter read this; the runner populates it from MergedItems
	// when policy_filter is disabled.
	AfterTrust []domain.ContextItem

	// Ranked is the rank stage output (and the input to
	// context_pack). Distinct from AfterTrust so explain traces
	// can attribute rank's reordering separately.
	Ranked []domain.ContextItem

	// Hits is the context_pack / build_grounded_hits output and the
	// value the facade hands back to the caller via Memory.Recall.
	Hits []domain.Hit

	// MaterializeDrops aggregates candidates the materialize step
	// discarded across sub-scopes (stale fact / superseded / scope
	// violation / retired). This is the authoritative inter-stage
	// signal for downstream consumers (evolution_after_recall); stages MUST read
	// from here rather than reaching into Trace.Stages so Trace can stay
	// diagnostic-only and be elided when the caller does not request
	// RecallExplain.
	MaterializeDrops []diagnostic.CandidateDrop

	// EvolutionErr captures a non-fatal AfterRecall failure surfaced by the
	// evolution_after_recall stage detail.
	EvolutionErr error

	// Trace is a DIAGNOSTIC artifact — it carries human-readable
	// StageDiagnostic entries for explainability. Stages MUST NOT
	// read information out of Trace; inter-stage signals belong on
	// State directly (see MaterializeDrops, Plan, etc.). This
	// separation lets Recall elide Trace allocation entirely when diagnostics
	// are not requested: Memory.Recall leaves Trace nil; Memory.RecallExplain
	// calls EnsureTrace so the framework writes per-stage diagnostics into
	// Trace.Stages via AppendStage.
	Trace *domain.RecallTrace
}

// SubScopeState is one sub-scope's slice of the federated read pipeline.
// candidate_fanout creates one per entry of scope.EffectiveFederation(); the
// non-federation path constructs a single SubScopeState wrapping Scope.
type SubScopeState struct {
	// Scope is this sub-scope's full qualifier. Stages that hit
	// the temporal store / projections use this; the top-level
	// ReadState.Scope is only the primary.
	Scope domain.Scope

	// Plan is the per-sub-scope plan. The strategy is copied from
	// the global Plan; only Intent.Scope changes per sub-scope.
	Plan *domain.QueryPlan

	// SourceResults captures each registered Source's
	// contribution. candidate_fanout populates it;
	// candidate_merge_and_materialize consumes it.
	SourceResults []domain.SourceResult

	// Candidates is the merged candidate set for this sub-scope.
	Candidates []domain.Candidate

	// CandidateDrops records source candidates discarded while constructing this
	// sub-scope's candidate pool. Stored per-sub-scope so
	// candidate_merge_and_materialize can roll per-scope attribution into one
	// cross-scope view.
	CandidateDrops []diagnostic.CandidateDrop

	// Materialized is the per-sub-scope materialized item set.
	// candidate_merge_and_materialize dedups by FactID across
	// sub-scopes and writes ReadState.MergedItems.
	Materialized []domain.ContextItem

	// MaterializeDrops records candidates the materializer
	// discarded (stale fact / superseded / scope violation).
	// Kept per-sub-scope for the same attribution reason as
	// FusionDrops.
	MaterializeDrops []diagnostic.CandidateDrop

	// FastPath is true when this sub-scope is the only entry in
	// SubScopeStates (len==1 single-scope read).
	FastPath bool
}

// EnsureTrace allocates the RecallTrace when explain output was
// requested. Memory.Recall (no diagnostics) leaves Trace nil so
// the hot path pays no per-stage allocation; Memory.RecallExplain
// (and tests that want to inspect the trace) call this so the
// framework's AppendStage hook has somewhere to write.
func (s *ReadState) EnsureTrace() *domain.RecallTrace {
	if s.Trace == nil {
		s.Trace = &domain.RecallTrace{}
	}
	return s.Trace
}

// CollectMaterializeDrops returns the aggregated set of materialize
// drops for the read pass. The top-level MaterializeDrops slot is
// preferred; when empty the helper falls back to concatenating the
// per-sub-scope MaterializeDrops written by
// candidate_merge_and_materialize. Consumers (e.g. evolution_after_recall)
// MUST use this helper instead of reaching into Trace.Stages so
// Trace stays optional for callers that do not request diagnostics.
func (s *ReadState) CollectMaterializeDrops() []diagnostic.CandidateDrop {
	if s == nil {
		return nil
	}
	if len(s.MaterializeDrops) > 0 {
		out := make([]diagnostic.CandidateDrop, len(s.MaterializeDrops))
		copy(out, s.MaterializeDrops)
		return out
	}
	var total int
	for i := range s.SubScopeStates {
		total += len(s.SubScopeStates[i].MaterializeDrops)
	}
	if total == 0 {
		return nil
	}
	out := make([]diagnostic.CandidateDrop, 0, total)
	for i := range s.SubScopeStates {
		out = append(out, s.SubScopeStates[i].MaterializeDrops...)
	}
	return out
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
// candidate_fanout guarantees the primary scope is at index 0;
// downstream stages use this helper to access "the canonical
// sub-scope" without re-implementing the lookup.
func (s *ReadState) PrimarySubScope() *SubScopeState {
	if s == nil || len(s.SubScopeStates) == 0 {
		return nil
	}
	return &s.SubScopeStates[0]
}
