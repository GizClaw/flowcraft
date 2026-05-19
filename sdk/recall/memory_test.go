package recall

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	retrievalproj "github.com/GizClaw/flowcraft/sdk/recall/internal/projection/retrieval"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestSave_AppendsAndProjects(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		WithRetrievalIndex(idx),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactRelation,
			Subject:   "Alice",
			Predicate: "spouse",
			Object:    "Bob",
		}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 || res.FactIDs[0] == "" {
		t.Fatalf("unexpected save result: %+v", res)
	}
	id := res.FactIDs[0]

	got, err := store.Get(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("store.Get after save: %v", err)
	}
	if got.MergeKey != "relation|alice|spouse|bob" {
		t.Errorf("merge_key = %q", got.MergeKey)
	}

	if _, ok, err := idx.Get(context.Background(), retrievalproj.NamespaceFor(scope), id); err != nil || !ok {
		t.Errorf("retrieval projection missing fact: ok=%v err=%v", ok, err)
	}
}

func TestSave_RequiresRuntimeID(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), Scope{}, SaveRequest{Facts: []TemporalFact{{Kind: FactNote, Content: "x"}}}); err == nil {
		t.Fatal("want error for missing runtime id")
	}
}

func TestSave_RequiredProjectionFailureAborts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		WithExtraProjection(failingProjection{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = mem.Save(context.Background(), Scope{RuntimeID: "rt"}, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "x"}},
	})
	if err == nil {
		t.Fatal("required projection failure must surface")
	}
	got, listErr := store.List(context.Background(), Scope{RuntimeID: "rt"}, temporalstore.ListQuery{})
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	if len(got) != 0 {
		t.Fatalf("failed Save must not leave canonical facts behind: %+v", got)
	}
}

func TestForget_RemovesFromStoreAndProjections(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, _ := New(WithTemporalStore(store), WithRetrievalIndex(idx))
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "burn after reading"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := res.FactIDs[0]
	if err := mem.Forget(context.Background(), scope, id); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := store.Get(context.Background(), scope, id); !errors.Is(err, temporalstore.ErrNotFound) {
		t.Errorf("store should be empty after forget, got %v", err)
	}
	if _, ok, _ := idx.Get(context.Background(), retrievalproj.NamespaceFor(scope), id); ok {
		t.Error("retrieval projection should be empty after forget")
	}
}

func TestForget_RequiredProjectionFailurePreservesCanonicalFact(t *testing.T) {
	idx := &deleteFailIndex{Index: retrievalmem.New()}
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store), WithRetrievalIndex(idx))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "keep if projection forget fails"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	id := res.FactIDs[0]

	if err := mem.Forget(context.Background(), scope, id); err == nil {
		t.Fatal("forget should surface required projection failure")
	}
	if _, err := store.Get(context.Background(), scope, id); err != nil {
		t.Fatalf("failed Forget must preserve canonical fact for retry/reconcile, got %v", err)
	}
}

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
	if len(trace.Sources) != 2 {
		t.Fatalf("want 2 sources in trace, got %d (%+v)", len(trace.Sources), trace.Sources)
	}
	gotNames := map[string]bool{}
	for _, s := range trace.Sources {
		gotNames[s.Source] = true
	}
	if !gotNames["retrieval"] || !gotNames["entity"] {
		t.Errorf("trace must cover retrieval and entity, got %+v", trace.Sources)
	}
	if trace.Materialized == 0 {
		t.Error("materialized count must be > 0")
	}
	if trace.Plan.TotalCap == 0 {
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
	mem, _ := New(WithSources(src))
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
	mem, _ := New(WithSources(errorSource{name: "retrieval", err: errors.New("retrieval unavailable")}))
	hits, err := mem.Recall(context.Background(), Scope{RuntimeID: "rt"}, Query{Text: "anything"})
	if err == nil {
		t.Fatalf("expected Recall to return an error when every selected source fails, hits=%+v", hits)
	}
}

// failingProjection is a required projection whose Project always
// fails. Used to verify Save aborts on required-projection failure.
type failingProjection struct{}

func (failingProjection) Name() string                        { return "broken" }
func (failingProjection) Consistency() projection.Consistency { return projection.Required }
func (failingProjection) Project(context.Context, []model.TemporalFact) error {
	return errors.New("synthetic")
}
func (failingProjection) Forget(context.Context, model.Scope, []string) error { return nil }
func (failingProjection) Rebuild(context.Context, model.Scope, []model.TemporalFact) error {
	return nil
}

type deleteFailIndex struct {
	*retrievalmem.Index
}

func (d *deleteFailIndex) Delete(context.Context, string, []string) error {
	return errors.New("synthetic delete failure")
}

type staticCandidateSource struct {
	name    string
	factIDs []string
}

func (s *staticCandidateSource) Name() string { return s.name }

func (s *staticCandidateSource) Query(_ context.Context, plan model.QueryPlan) model.SourceResult {
	candidates := make([]model.Candidate, 0, len(s.factIDs))
	for i, id := range s.factIDs {
		candidates = append(candidates, model.Candidate{
			FactID: id,
			Scope:  plan.Intent.Scope,
			Source: s.name,
			Rank:   i + 1,
			Score:  1,
		})
	}
	return model.SourceResult{Source: s.name, Candidates: candidates}
}

type errorSource struct {
	name string
	err  error
}

func (s errorSource) Name() string { return s.name }

func (s errorSource) Query(context.Context, model.QueryPlan) model.SourceResult {
	return model.SourceResult{Source: s.name, Err: s.err}
}
