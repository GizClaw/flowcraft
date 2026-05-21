package recall

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/diagnostics"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	retrievallens "github.com/GizClaw/flowcraft/sdk/recall/internal/lens/retrieval"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	retrievalmem "github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestSave_AppendsAndProjects(t *testing.T) {
	idx := retrievalmem.New()
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		withTemporalStore(store),
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

	if _, ok, err := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); err != nil || !ok {
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
		withTemporalStore(store),
		withExtraProjection(failingProjection{}),
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
	got, listErr := store.List(context.Background(), Scope{RuntimeID: "rt"}, port.ListQuery{})
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
	mem, _ := New(withTemporalStore(store), WithRetrievalIndex(idx))
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
	if _, ok, _ := idx.Get(context.Background(), retrievallens.NamespaceFor(scope), id); ok {
		t.Error("retrieval projection should be empty after forget")
	}
}

func TestSave_StateSecondWriteSupersedesPrior(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(withTemporalStore(store))
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	res1, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	if len(res1.FactIDs) != 1 {
		t.Fatalf("first save returned %d ids", len(res1.FactIDs))
	}
	priorID := res1.FactIDs[0]

	res2, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Berlin",
		}},
	})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if len(res2.FactIDs) != 1 {
		t.Fatalf("second save returned %d ids", len(res2.FactIDs))
	}
	successorID := res2.FactIDs[0]

	prior, err := store.Get(context.Background(), scope, priorID)
	if err != nil {
		t.Fatalf("store.Get prior: %v", err)
	}
	if prior.ValidTo == nil {
		t.Fatalf("prior fact ValidTo should be set after supersede")
	}
	if prior.CorrectedBy != successorID {
		t.Errorf("prior.CorrectedBy = %q, want %q", prior.CorrectedBy, successorID)
	}

	successor, err := store.Get(context.Background(), scope, successorID)
	if err != nil {
		t.Fatalf("store.Get successor: %v", err)
	}
	if len(successor.Supersedes) != 1 || successor.Supersedes[0] != priorID {
		t.Errorf("successor.Supersedes = %v, want [%q]", successor.Supersedes, priorID)
	}

	// Recall should surface only the successor.
	hits, trace, err := mem.(RecallExplainer).RecallExplain(context.Background(), scope, Query{
		Text:     "Paris",
		Entities: []string{"alice"},
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Fact.ID == priorID {
			t.Errorf("superseded fact must not appear in Recall, got %+v", hits)
		}
	}
	for _, drop := range diagnostics.Drops(trace) {
		if drop.FactID == priorID && drop.Reason == DropSuperseded {
			t.Fatalf("required projections should not emit superseded candidates after normal Save, drops=%+v", diagnostics.Drops(trace))
		}
	}
}

func TestSave_StateSecondWriteIdenticalContentIsNoop(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(withTemporalStore(store))
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("noop save: %v", err)
	}
	if len(second.FactIDs) != 0 {
		t.Errorf("identical content save must be noop, got ids=%v", second.FactIDs)
	}
	// First fact is still active.
	prior, _ := store.Get(context.Background(), scope, first.FactIDs[0])
	if prior.CorrectedBy != "" {
		t.Errorf("prior fact must remain active, CorrectedBy=%q", prior.CorrectedBy)
	}
	if prior.ValidTo != nil {
		t.Errorf("prior fact must remain active, ValidTo=%v", *prior.ValidTo)
	}
}

func TestSave_ConcurrentStateUpdatesSerializeByScope(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(withTemporalStore(store))
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "city-00",
		}},
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	const updates = 24
	start := make(chan struct{})
	errs := make(chan error, updates)
	var wg sync.WaitGroup
	for i := 0; i < updates; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := mem.Save(context.Background(), scope, SaveRequest{
				Facts: []TemporalFact{{
					Kind: FactState, Subject: "alice", Predicate: "city",
					Content: "city-" + time.Unix(int64(i+1), 0).Format("05"),
				}},
			})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent save failed: %v", err)
		}
	}

	facts, err := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	active := 0
	for _, f := range facts {
		if f.Kind != FactState || f.Subject != "alice" || f.Predicate != "city" {
			continue
		}
		if f.CorrectedBy == "" && f.ValidTo == nil {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active city facts = %d, want 1; facts=%+v", active, facts)
	}
}

func TestSave_StateUpdatesChainWithinSingleBatch(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(withTemporalStore(store))
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("seed save: %v", err)
	}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{
			{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Berlin"},
			{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Rome"},
		},
	})
	if err != nil {
		t.Fatalf("batch save: %v", err)
	}
	if len(res.FactIDs) != 2 {
		t.Fatalf("ids = %+v", res.FactIDs)
	}
	old, _ := store.Get(context.Background(), scope, first.FactIDs[0])
	mid, _ := store.Get(context.Background(), scope, res.FactIDs[0])
	latest, _ := store.Get(context.Background(), scope, res.FactIDs[1])
	if old.CorrectedBy != res.FactIDs[0] {
		t.Fatalf("old.CorrectedBy = %q, want %q", old.CorrectedBy, res.FactIDs[0])
	}
	if mid.CorrectedBy != res.FactIDs[1] {
		t.Fatalf("mid.CorrectedBy = %q, want %q", mid.CorrectedBy, res.FactIDs[1])
	}
	if latest.CorrectedBy != "" || latest.ValidTo != nil {
		t.Fatalf("latest should be active: %+v", latest)
	}
}

// TestSave_TolerantOfRaceSupersedeClose simulates two memory
// instances sharing one store racing to supersede the same prior
// state fact. The first reaches UpdateValidity and wins; the second
// must NOT fail Save just because the prior's CorrectedBy got
// claimed by a different (semantically equivalent) successor — the
// race-loser's new fact still gets appended with its Supersedes
// pointer, so the supersede chain stays reconstructable. This is
// the safety net for the cross-instance race that triggered
// "fact validity already closed" WARNs in long concurrent ingests.
func TestSave_TolerantOfRaceSupersedeClose(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	memA, _ := New(withTemporalStore(store))
	if _, err := memA.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Second memory shares the same store but has its own write lock,
	// so concurrent Saves emulate two replicas with no cross-process
	// serialization.
	memB, _ := New(withTemporalStore(store))

	resA, err := memA.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Berlin",
		}},
	})
	if err != nil {
		t.Fatalf("memA save: %v", err)
	}

	// memB still has the resolver looking at the pre-supersede view
	// because it never observed memA's close — emulate that by saving
	// a different successor for the SAME merge_key. memB's resolver
	// will compute a close against the prior fact that memA already
	// closed. Without tolerance, this Save fails.
	resB, err := memB.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Rome",
		}},
	})
	if err != nil {
		t.Fatalf("memB save (race close): %v", err)
	}
	if len(resA.FactIDs) != 1 || len(resB.FactIDs) != 1 {
		t.Fatalf("expected one fact per save, got %+v / %+v", resA.FactIDs, resB.FactIDs)
	}

	// memB's new fact must still carry the Supersedes pointer.
	got, err := store.Get(context.Background(), scope, resB.FactIDs[0])
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if len(got.Supersedes) == 0 {
		t.Errorf("memB fact should record what it supersedes, got %+v", got.Supersedes)
	}
}

// TestStore_ErrValidityAlreadyClosed_HasSentinelIdentity pins the
// classification: the sentinel must still satisfy errors.Is so the
// Save tolerance path matches, and IsConflict so callers that DO
// want strict semantics keep their behavior.
func TestStore_ErrValidityAlreadyClosed_HasSentinelIdentity(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt"}
	fact := domain.TemporalFact{
		ID: "a", Scope: scope, Kind: domain.KindState,
		Subject: "alice", Predicate: "city", Content: "Paris",
		ObservedAt: time.Unix(1, 0),
	}
	if err := store.Append(ctx, []domain.TemporalFact{fact}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateValidity(ctx, scope, "a", time.Unix(10, 0), "b"); err != nil {
		t.Fatal(err)
	}
	err := store.UpdateValidity(ctx, scope, "a", time.Unix(20, 0), "c")
	if err == nil {
		t.Fatal("want re-close mismatch error")
	}
	if !errors.Is(err, temporalstore.ErrValidityAlreadyClosed) {
		t.Errorf("errors.Is(err, ErrValidityAlreadyClosed) lost: %v", err)
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("errdefs.IsConflict(err) lost: %v", err)
	}
}

func TestSave_EventIsAlwaysAppendOnly(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(withTemporalStore(store))
	scope := Scope{RuntimeID: "rt"}

	for i := 0; i < 2; i++ {
		_, err := mem.Save(context.Background(), scope, SaveRequest{
			Facts: []TemporalFact{{
				Kind: FactEvent, Content: "ate ramen",
			}},
		})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	list, err := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("events must always append, got %d facts", len(list))
	}
	for _, f := range list {
		if f.CorrectedBy != "" {
			t.Errorf("event fact must never be superseded: %+v", f)
		}
	}
}

func TestSave_AliasResolverFoldsMentions(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	cp := ingest.New(ingest.Stages{
		AliasResolver: ingest.NewStaticAliasResolver(ingest.ScopeAliasEntry{
			Scope: scope, Aliases: map[string]string{"Bob": "robert"},
		}),
	})
	mem, _ := New(withTemporalStore(store), withCompiler(cp))

	_, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactRelation, Subject: "Alice", Predicate: "spouse", Object: "Bob",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := store.List(context.Background(), scope, port.ListQuery{})
	if len(list) != 1 {
		t.Fatalf("want 1 fact, got %d", len(list))
	}
	if list[0].Object != "robert" {
		t.Errorf("object not aliased: %q", list[0].Object)
	}
	if list[0].MergeKey != "relation|alice|spouse|robert" {
		t.Errorf("merge_key did not pick up alias: %q", list[0].MergeKey)
	}
}

func TestSave_TimeResolverConsumesHint(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	scope := Scope{RuntimeID: "rt"}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cp := ingest.New(ingest.Stages{
		Clock: func() time.Time { return now },
	})
	mem, _ := New(withTemporalStore(store), withCompiler(cp))

	_, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:    FactPlan,
			Content: "visit Paris",
			Metadata: map[string]any{
				ingest.MetaValidFromHint: "tomorrow",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := store.List(context.Background(), scope, port.ListQuery{})
	if len(list) != 1 {
		t.Fatal("expected 1 plan")
	}
	wantDate := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	if list[0].ValidFrom == nil || !list[0].ValidFrom.Equal(wantDate) {
		t.Errorf("ValidFrom = %v, want %v", list[0].ValidFrom, wantDate)
	}
	if _, leftover := list[0].Metadata[ingest.MetaValidFromHint]; leftover {
		t.Error("hint should have been consumed from metadata")
	}
}

func TestSave_ProjectionFailureAfterSupersedeRestoresPriorFact(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	proj := &failOnProjectN{failOn: 2}
	mem, err := New(withTemporalStore(store), withExtraProjection(proj))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	priorID := first.FactIDs[0]

	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Berlin",
		}},
	})
	if err == nil {
		t.Fatal("second save should fail at required projection")
	}

	prior, err := store.Get(context.Background(), scope, priorID)
	if err != nil {
		t.Fatalf("prior fact should still exist: %v", err)
	}
	if prior.CorrectedBy != "" || prior.ValidTo != nil {
		t.Fatalf("failed superseding save must leave prior fact active, got %+v", prior)
	}
	list, err := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != priorID {
		t.Fatalf("failed superseding save must roll back new fact only, got %+v", list)
	}
}

func TestSave_CrossAgentSameMergeKeyDoesNotSupersedeOtherAgentFact(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	agentA := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	agentB := Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}

	first, err := mem.Save(context.Background(), agentB, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Paris",
		}},
	})
	if err != nil {
		t.Fatalf("agent-b save: %v", err)
	}
	if _, err := mem.Save(context.Background(), agentA, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Berlin",
		}},
	}); err != nil {
		t.Fatalf("agent-a save: %v", err)
	}

	bFact, err := store.Get(context.Background(), agentB, first.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if bFact.CorrectedBy != "" || bFact.ValidTo != nil {
		t.Fatalf("agent-a write must not close agent-b private fact, got %+v", bFact)
	}
}

func TestSaveRequest_ExposesTurnsForLLMExtraction(t *testing.T) {
	if _, ok := reflect.TypeOf(SaveRequest{}).FieldByName("Turns"); !ok {
		t.Fatal("SaveRequest must expose Turns so opt-in LLM extractors can be reached through Memory.Save")
	}
}

func TestForget_RequiredProjectionFailurePreservesCanonicalFact(t *testing.T) {
	idx := &deleteFailIndex{Index: retrievalmem.New()}
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithRetrievalIndex(idx))
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

// TestRecall_NoDiagnostics_TraceNil pins Cluster F (2026-05-21):
// the Recall hot path MUST NOT allocate a state.Trace when the
// caller did not ask for diagnostics. We can't introspect state
// directly (it is internal), so we use the public observation that
// `Recall` returns no RecallTrace and that hits are still correct
// — the captureEvolution recall hook (which used to depend on
// trace.Stages being populated) is exercised separately by
// TestRecall_WithDiagnostics_TraceMatches.
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
// counters. This is the Cluster F (2026-05-21) opposite of the
// nil-trace fast path.
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
	if diagnostics.Materialized(trace) == 0 {
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
	if diagnostics.Materialized(trace) == 0 {
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

// failingProjection is a required projection whose Project always
// fails. Used to verify Save aborts on required-projection failure.
type failingProjection struct{}

func (failingProjection) Name() string                  { return "broken" }
func (failingProjection) Consistency() port.Consistency { return port.Required }
func (failingProjection) Project(context.Context, []domain.TemporalFact) error {
	return errors.New("synthetic")
}
func (failingProjection) Forget(context.Context, domain.Scope, []string) error { return nil }
func (failingProjection) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
}
func (failingProjection) ClearScope(context.Context, domain.Scope) error { return nil }

type deleteFailIndex struct {
	*retrievalmem.Index
}

func (d *deleteFailIndex) Delete(context.Context, string, []string) error {
	return errors.New("synthetic delete failure")
}

// scriptedLLM is a minimal llm.LLM for testing the WithLLMExtractor
// facade option. It returns the configured Response on every
// Generate call and records the options it received so tests can
// verify the extractor pipeline wired them correctly.
type scriptedLLM struct {
	Response string
	Options  [][]llm.GenerateOption
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	s.Options = append(s.Options, opts)
	body := s.Response
	if body == "" {
		body = `{"facts":[]}`
	}
	return llm.NewTextMessage(llm.RoleAssistant, body), llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, errors.New("scriptedLLM: streaming not implemented")
}

func TestWithLLMExtractor_WiresExtractorIntoSavePath(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	client := &scriptedLLM{Response: `{"facts":[{
		"kind":"preference",
		"subject":"alice",
		"predicate":"city",
		"content":"Paris",
		"source_message_ids":["D1:3"],
		"evidence_refs":[{"id":"D1:3","message_id":"m-3","role":"user","text":"Alice said Paris is her city.","timestamp":"2026-05-19T05:00:00Z"}]
	}]}`}

	mem, err := New(
		withTemporalStore(store),
		WithLLMExtractor(
			client,
			WithLLMExtractorTemperature(0.2),
			WithLLMExtractorSchemaName("recall_facts_v1"),
		),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Turns: []TurnContext{{ID: "D1:3", Role: "user", Text: "Alice said Paris is her city."}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Fatalf("save returned %d ids", len(res.FactIDs))
	}
	fact, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if len(fact.EvidenceRefs) != 1 || fact.EvidenceRefs[0].ID != "D1:3" {
		t.Fatalf("LLM-extracted evidence refs not persisted: %+v", fact.EvidenceRefs)
	}
	if len(fact.SourceMessageIDs) != 1 || fact.SourceMessageIDs[0] != "D1:3" {
		t.Fatalf("source message ids not persisted: %+v", fact.SourceMessageIDs)
	}
	if len(client.Options) == 0 {
		t.Fatal("expected at least one LLM call to record options")
	}
	last := client.Options[len(client.Options)-1]
	got := llm.GenerateOptions{}
	for _, opt := range last {
		opt(&got)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("temperature option not propagated, got=%v", got.Temperature)
	}
	if got.JSONSchema == nil || got.JSONSchema.Name != "recall_facts_v1" {
		t.Errorf("schema name option not propagated, got=%+v", got.JSONSchema)
	}
	if got.JSONMode == nil || !*got.JSONMode {
		t.Errorf("JSON mode should be enabled")
	}
}

func TestWithLLMExtractor_IgnoredWhenCompilerProvided(t *testing.T) {
	client := &scriptedLLM{}
	customCompiler := ingest.Default()

	mem, err := New(
		withCompiler(customCompiler),
		WithLLMExtractor(client),
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// With both options set, the custom compiler wins. Sanity:
	// we should be able to use Memory without panic. We don't
	// expose the compiler externally so the only check we can
	// make is that LLM was never invoked through this path.
	scope := Scope{RuntimeID: "rt"}
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "noop"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(client.Options) != 0 {
		t.Errorf("LLM extractor must be ignored when a compiler is supplied, calls=%d", len(client.Options))
	}
}

func TestWithLLMExtractor_NilClientFallsBackToPassthrough(t *testing.T) {
	mem, err := New(WithLLMExtractor(nil))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope := Scope{RuntimeID: "rt"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "still works"}},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(res.FactIDs) != 1 {
		t.Errorf("nil LLM client should not break the default compiler, got %d ids", len(res.FactIDs))
	}
}

// These tests pin the public-boundary error contract for sdk/recall
// v2: Save / Recall / Forget input validation must be classifiable
// as errdefs.Validation so HTTP/gRPC shims map to 400 without
// each caller pattern-matching error text.

func TestSave_MissingRuntimeID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	_, err = mem.Save(context.Background(), Scope{}, SaveRequest{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id on Save must map to Validation: %v", err)
	}
}

func TestRecall_MissingRuntimeID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	_, err = mem.Recall(context.Background(), Scope{}, Query{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id on Recall must map to Validation: %v", err)
	}
}

func TestForget_EmptyFactID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	err = mem.Forget(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, "")
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("empty fact id on Forget must map to Validation: %v", err)
	}
}

func TestSave_TierCoreBoostsConfidence(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}

	ex, ok := mem.(SaveExplainer)
	if !ok {
		t.Fatal("memory does not implement SaveExplainer")
	}
	resGeneral, _, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier general"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resCore, _, err := ex.SaveExplain(context.Background(), scope, SaveRequest{
		Tier:  domain.TierCore,
		Facts: []TemporalFact{{Kind: FactNote, Content: "tier core"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resGeneral.FactIDs) != 1 || len(resCore.FactIDs) != 1 {
		t.Fatalf("fact ids general=%d core=%d", len(resGeneral.FactIDs), len(resCore.FactIDs))
	}
	confGeneral := factConfidence(mem, scope, resGeneral.FactIDs[0], "tier general")
	confCore := factConfidence(mem, scope, resCore.FactIDs[0], "tier core")
	if confCore <= confGeneral {
		t.Fatalf("core confidence %v want > general %v", confCore, confGeneral)
	}
}

func factConfidence(mem Memory, scope Scope, id, text string) float64 {
	hits, err := mem.Recall(context.Background(), scope, Query{Text: text, Limit: 5})
	if err != nil {
		panic(err)
	}
	for _, h := range hits {
		if h.Fact.ID == id {
			return h.Fact.Confidence
		}
	}
	return 0
}

func TestFork_KeepsPriorActive(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind:      FactState,
			Subject:   "alice",
			Predicate: "location",
			Object:    "paris",
			Content:   "alice in paris",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	priorID := first.FactIDs[0]
	if _, err = mem.Fork(context.Background(), scope, priorID, TemporalFact{
		Kind:      FactState,
		Subject:   "alice",
		Predicate: "location",
		Object:    "lyon",
		Content:   "alice in lyon",
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "alice location", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var seenPrior, seenFork bool
	for _, h := range hits {
		if h.Fact.ID == priorID {
			seenPrior = true
		}
		if h.Fact.Object == "lyon" {
			seenFork = true
		}
	}
	if !seenPrior || !seenFork {
		t.Fatalf("fork recall: prior=%v fork=%v hits=%d", seenPrior, seenFork, len(hits))
	}
}

func TestTrustFilter_RemovesSecretFacts(t *testing.T) {
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

func TestReinforce_BoostsRank(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	low, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha beta gamma", Confidence: 0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Tier:  domain.TierCore,
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha beta gamma other", Confidence: 0.5}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.Reinforce(context.Background(), scope, low.FactIDs[0], 2.0); err != nil {
		t.Fatal(err)
	}
	hits, err := mem.Recall(context.Background(), scope, Query{Text: "alpha beta gamma", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Fact.ID != low.FactIDs[0] {
		t.Fatalf("top hit = %s want reinforced %s", hits[0].Fact.ID, low.FactIDs[0])
	}
}

type failOnProjectN struct {
	n      int
	failOn int
}

func (p *failOnProjectN) Name() string { return "fail_on_project_n" }

func (p *failOnProjectN) Consistency() port.Consistency { return port.Required }

func (p *failOnProjectN) Project(context.Context, []domain.TemporalFact) error {
	p.n++
	if p.n == p.failOn {
		return errors.New("synthetic project failure")
	}
	return nil
}

func (p *failOnProjectN) Forget(context.Context, domain.Scope, []string) error { return nil }

func (p *failOnProjectN) ClearScope(context.Context, domain.Scope) error { return nil }

func (p *failOnProjectN) Rebuild(context.Context, domain.Scope, []domain.TemporalFact) error {
	return nil
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

// TestReinforce_RoutesToFeedbackPipeline pins the Cluster A
// 2026-05-21 contract: Memory.Reinforce runs through the feedback
// pipeline so the call emits a single apply_feedback StageDiagnostic
// AND the canonical fact's reinforcement counter advances. The
// retrieval projection sees the updated MetaReinforcement on the
// follow-up reproject (Cluster D fix).
func TestReinforce_RoutesToFeedbackPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	if err := mem.Reinforce(context.Background(), scope, res.FactIDs[0], 3); err != nil {
		t.Fatalf("Reinforce: %v", err)
	}
	if !hasStage(hook.stages, "apply_feedback") {
		t.Errorf("expected apply_feedback stage diagnostic; got %v", stageNames(hook.stages))
	}
	got, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if got.Reinforcement != 3 {
		t.Errorf("Reinforcement = %v, want 3", got.Reinforcement)
	}
}

// TestPenalize_RoutesToFeedbackPipeline is the negative-channel
// symmetric of the Reinforce route test.
func TestPenalize_RoutesToFeedbackPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	res, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	if err := mem.Penalize(context.Background(), scope, res.FactIDs[0], 1.5); err != nil {
		t.Fatalf("Penalize: %v", err)
	}
	if !hasStage(hook.stages, "apply_feedback") {
		t.Errorf("expected apply_feedback stage; got %v", stageNames(hook.stages))
	}
	got, _ := store.Get(context.Background(), scope, res.FactIDs[0])
	if got.Penalty != 1.5 {
		t.Errorf("Penalty = %v, want 1.5", got.Penalty)
	}
}

// TestFork_RoutesToRevisionPipeline pins that Memory.Fork emits the
// three revision pipeline stages (lookup_source / attach / save) AND
// produces a new canonical fact carrying the RevisionFork annotation
// while leaving the source fact active.
func TestFork_RoutesToRevisionPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	res, err := mem.Fork(context.Background(), scope, first.FactIDs[0], TemporalFact{
		Kind:      FactState,
		Subject:   "alice",
		Predicate: "city",
		Content:   "Lyon",
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	for _, name := range []string{"revision_lookup_source", "revision_attach", "revision_save"} {
		if !hasStage(hook.stages, name) {
			t.Errorf("expected stage %q; got %v", name, stageNames(hook.stages))
		}
	}
	created, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	rev, ok := domain.RevisionOf(created)
	if !ok || rev.Kind != domain.RevisionFork || rev.SourceFactID != first.FactIDs[0] {
		t.Errorf("Revision = %+v ok=%v, want fork/%s", rev, ok, first.FactIDs[0])
	}
	prior, _ := store.Get(context.Background(), scope, first.FactIDs[0])
	if prior.ValidTo != nil || prior.CorrectedBy != "" {
		t.Errorf("Fork must NOT close source; got ValidTo=%v CorrectedBy=%q", prior.ValidTo, prior.CorrectedBy)
	}
}

// TestContest_RoutesToRevisionPipeline pins that Memory.Contest
// emits the three revision pipeline stages and creates a FactNote
// carrying the RevisionContest annotation.
func TestContest_RoutesToRevisionPipeline(t *testing.T) {
	hook := &captureHook{}
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store), WithTelemetryHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	first, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactState, Subject: "alice", Predicate: "city", Content: "Paris"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook.stages = nil

	res, err := mem.Contest(context.Background(), scope, first.FactIDs[0], []EvidenceRef{{ID: "ev-1"}})
	if err != nil {
		t.Fatalf("Contest: %v", err)
	}
	for _, name := range []string{"revision_lookup_source", "revision_attach", "revision_save"} {
		if !hasStage(hook.stages, name) {
			t.Errorf("expected stage %q; got %v", name, stageNames(hook.stages))
		}
	}
	created, err := store.Get(context.Background(), scope, res.FactIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != FactNote {
		t.Errorf("Contest fact kind = %v, want FactNote", created.Kind)
	}
	rev, ok := domain.RevisionOf(created)
	if !ok || rev.Kind != domain.RevisionContest || rev.SourceFactID != first.FactIDs[0] {
		t.Errorf("Revision = %+v ok=%v, want contest/%s", rev, ok, first.FactIDs[0])
	}
}

// TestExpireRetired_HardDeletesExpiredFacts pins the D5 2026-05-21
// API: ExpireRetired hard-deletes facts whose ExpiresAt is in the
// past relative to the supplied cutoff and leaves the rest intact.
func TestExpireRetired_HardDeletesExpiredFacts(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(withTemporalStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	for i, exp := range []*time.Time{&past, &past, &past, &future, &future} {
		fact := TemporalFact{
			Kind:      FactNote,
			Content:   "f" + string(rune('a'+i)),
			ExpiresAt: exp,
		}
		if _, err := mem.Save(context.Background(), scope, SaveRequest{Facts: []TemporalFact{fact}}); err != nil {
			t.Fatalf("seed save %d: %v", i, err)
		}
	}

	deleted, err := mem.ExpireRetired(context.Background(), scope, now)
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}
	got, _ := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if len(got) != 2 {
		t.Errorf("surviving facts = %d, want 2", len(got))
	}
}

// TestExpireRetired_NoExpiredFactsReturnsZero pins the empty-sweep
// contract: a clean scope returns (0, nil) without touching the
// store or projections.
func TestExpireRetired_NoExpiredFactsReturnsZero(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	if _, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "alpha"}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := mem.ExpireRetired(context.Background(), scope, time.Now())
	if err != nil {
		t.Fatalf("ExpireRetired: %v", err)
	}
	if got != 0 {
		t.Errorf("got = %d, want 0", got)
	}
}
