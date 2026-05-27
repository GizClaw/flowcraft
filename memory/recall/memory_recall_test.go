package recall

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestRecall_FindsFactByText(t *testing.T) {
	mem, _ := New()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactNote, Content: "Alice loves Paris croissants"},
			{Kind: FactNote, Content: "Bob hates Berlin weather"},
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	drainSideEffectsForTest(t, mem, scope)
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "Paris"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit for 'Paris'")
	}
	if got := hits[0].Fact.Content; got != "Alice loves Paris croissants" {
		t.Errorf("top hit = %q", got)
	}
}

func TestRecall_RequiresRuntimeID(t *testing.T) {
	mem, _ := New()
	if _, err := mem.Recall(context.Background(), Scope{}, Query{Text: "x"}); err == nil {
		t.Fatal("expected error for missing runtime id")
	}
}

func TestRecall_EntitySourceFiresOnlyWithHints(t *testing.T) {
	mem, _ := New()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactNote, Content: "Charlie's favourite city is Tokyo", Entities: []string{"charlie"}},
			{Kind: FactNote, Content: "Random unrelated", Entities: []string{"diana"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	// No entity hint -> only retrieval contributes; both unrelated
	// strings can still match via BM25 zero-length match. Use a
	// text that matches Charlie's content so we get a deterministic
	// retrieval hit.
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "Tokyo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Fact.Content != "Charlie's favourite city is Tokyo" {
		t.Fatalf("retrieval-only recall = %+v", hits)
	}

	// Entity hint with no text still finds Charlie via the entity
	// projection.
	hits, err = mem.Recall(context.Background(), scope, Query{Entities: []string{"charlie"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Fact.Content != "Charlie's favourite city is Tokyo" {
		t.Fatalf("entity-only recall = %+v", hits)
	}
}

func TestRecall_ForgottenFactDoesNotSurface(t *testing.T) {
	mem, _ := New()
	scope := Scope{RuntimeID: "rt"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "fleeting thought"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)
	if err := mem.Forget(context.Background(), scope, res.FactIDs[0]); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "fleeting"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("forgotten fact must not surface, got %+v", hits)
	}
}

// TestRecall_NoDiagnostics_TraceNil pins that the Recall hot path
// MUST NOT allocate a state.Trace when the caller did not ask for
// diagnostics. We can't introspect state directly (it is internal), so
// we use the public observation that `Recall` returns no RecallTrace
// and that hits are still correct — the captureEvolution recall hook
// (which used to depend on trace.Stages being populated) is exercised
// separately by TestRecall_WithDiagnostics_TraceMatches.
func TestRecall_NoDiagnostics_TraceNil(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "Alice loves Paris", Entities: []string{"alice"}}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)

	hits, err := mem.Recall(context.Background(), scope, Query{Text: "Paris", Limit: 3})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(hits) == 0 || hits[0].Fact.Content != "Alice loves Paris" {
		t.Fatalf("Recall hits = %+v, want the Paris note on top", hits)
	}

	// The public Recall signature only returns hits + err — there is
	// no trace return, which IS the contract: diagnostics are
	// opt-in via RecallExplain. We additionally verify the
	// surrounding behavior (subsequent Recall calls keep working,
	// no panics on a nil-trace pipeline path).
	if _, err := mem.Recall(context.Background(), scope, Query{Text: "Paris"}); err != nil {
		t.Fatalf("second Recall must succeed on nil-trace path: %v", err)
	}
}

// TestRecall_WithDiagnostics_TraceMatches anchors the
// "diagnostics requested" branch: when the caller uses
// RecallExplain the framework MUST allocate Trace and the returned
// trace MUST carry stage diagnostics, including materialize-derived
// counters. This is the opposite of the nil-trace fast path.
func TestRecall_WithDiagnostics_TraceMatches(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "Alice loves Paris", Entities: []string{"alice"}}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)
	explainer, ok := mem.(RecallExplainer)
	if !ok {
		t.Fatal("Memory must implement RecallExplainer")
	}
	hits, trace, err := explainer.RecallExplain(context.Background(), scope, Query{Text: "Paris", Entities: []string{"alice"}})
	if err != nil {
		t.Fatalf("RecallExplain: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit on the diagnostics path")
	}
	if len(trace.Stages) == 0 {
		t.Fatal("diagnostics requested but trace.Stages is empty")
	}
	if diagnostics.MaterializedCount(trace) == 0 {
		t.Fatal("trace must carry materialize counters when diagnostics requested")
	}
}

func TestRecallExplain_PopulatesTrace(t *testing.T) {
	mem, _ := New()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactNote, Content: "Alice loves Paris", Entities: []string{"alice"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, scope)
	explainer, ok := mem.(RecallExplainer)
	if !ok {
		t.Fatal("Memory should implement RecallExplainer")
	}
	hits, trace, err := explainer.RecallExplain(context.Background(), scope, Query{Text: "Paris", Entities: []string{"alice"}})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected hits")
	}
	if len(diagnostics.Sources(trace)) != 2 {
		t.Fatalf("want 2 sources in trace, got %d (%+v)", len(diagnostics.Sources(trace)), diagnostics.Sources(trace))
	}
	gotNames := map[string]bool{}
	for _, s := range diagnostics.Sources(trace) {
		gotNames[s.Source] = true
	}
	if !gotNames["retrieval"] || !gotNames["entity"] {
		t.Errorf("trace must cover retrieval and entity, got %+v", diagnostics.Sources(trace))
	}
	if diagnostics.MaterializedCount(trace) == 0 {
		t.Error("materialized count must be > 0")
	}
	if diagnostics.Plan(trace).TotalCap == 0 {
		t.Error("plan TotalCap must be populated")
	}
}

func TestRecall_AgentIDSoftIsolation(t *testing.T) {
	mem, _ := New()
	base := Scope{RuntimeID: "rt", UserID: "u1"}
	agentA := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	agentB := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}

	// Two agent-owned facts plus one shared.
	if _, err := mem.Save(context.Background(), agentA, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "agent-a secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), agentB, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "agent-b secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), base, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "shared note"}},
	}); err != nil {
		t.Fatal(err)
	}
	drainSideEffectsForTest(t, mem, agentA)
	drainSideEffectsForTest(t, mem, agentB)
	drainSideEffectsForTest(t, mem, base)

	// agent-a query: must see its own secret + shared, NOT agent-b
	// secret.
	hits, err := mem.Recall(context.Background(), agentA, Query{Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Fact.Content == "agent-b secret" {
			t.Fatalf("agent-a query must not see agent-b secret, got %+v", hits)
		}
	}
	sawOwn := false
	for _, h := range hits {
		if h.Fact.Content == "agent-a secret" {
			sawOwn = true
		}
	}
	if !sawOwn {
		t.Errorf("agent-a query must see its own secret, got %+v", hits)
	}

	// cross-agent query (AgentID empty): sees everything.
	hits, err = mem.Recall(context.Background(), base, Query{Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Errorf("cross-agent recall should see >=2 secrets, got %+v", hits)
	}
}

func TestRecall_MaterializeEnforcesAgentIDSoftIsolationForAllSources(t *testing.T) {
	src := &staticCandidateSource{name: "retrieval"}
	mem, _ := New(withSources(src))
	agentA := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	agentB := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}

	res, err := mem.Save(context.Background(), agentB, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "agent-b private note"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	src.factIDs = []string{res.FactIDs[0]}

	hits, err := mem.Recall(context.Background(), agentA, Query{Text: "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("materialization must drop candidates outside AgentID soft isolation, got %+v", hits)
	}
}

func TestRecall_AllSourcesFailReturnsError(t *testing.T) {
	mem, _ := New(withSources(errorSource{name: "retrieval", err: errors.New("retrieval unavailable")}))
	hits, err := mem.Recall(context.Background(), Scope{RuntimeID: "rt"}, Query{Text: "anything"})
	if err == nil {
		t.Fatalf("expected Recall to return an error when every selected source fails, hits=%+v", hits)
	}
}

func TestPolicyFilter_RemovesSecretFacts(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:     FactNote,
			Content:  "secret plan",
			Metadata: map[string]any{domain.MetaSensitivity: "secret"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:     FactNote,
			Content:  "public note",
			Metadata: map[string]any{domain.MetaSensitivity: "public"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{
		Text:  "plan note",
		Limit: 10,
		Trust: &TrustContext{MaxSensitivity: "internal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if lab, _ := h.Fact.Metadata[domain.MetaSensitivity].(string); lab == "secret" {
			t.Fatalf("secret fact leaked: %+v", h.Fact)
		}
	}
}

type staticCandidateSource struct {
	name    string
	factIDs []string
}

func (s *staticCandidateSource) Name() string { return s.name }

func (s *staticCandidateSource) Query(_ context.Context, plan domain.QueryPlan) domain.SourceResult {
	candidates := make([]domain.Candidate, 0, len(s.factIDs))
	for i, id := range s.factIDs {
		candidates = append(candidates, domain.Candidate{
			FactID: id,
			Scope:  plan.Intent.Scope,
			Source: s.name,
			Rank:   i + 1,
			Score:  1,
		})
	}
	return domain.SourceResult{Source: s.name, Candidates: candidates}
}

type errorSource struct {
	name string
	err  error
}

func (s errorSource) Name() string { return s.name }

func (s errorSource) Query(context.Context, domain.QueryPlan) domain.SourceResult {
	return domain.SourceResult{Source: s.name, Err: s.err}
}
