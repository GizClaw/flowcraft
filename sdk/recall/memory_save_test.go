package recall

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
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
	drainSideEffectsForTest(t, mem, scope)

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

func TestSave_RequiredProjectionFailureRetriesSideEffect(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(
		WithTemporalStore(store),
		withExtraProjection(failingProjection{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{RuntimeID: "rt", UserID: "u1"}
	_, err = mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{Kind: FactNote, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Save should accept canonical write before side-effect processing: %v", err)
	}
	out := drainSideEffectsForTest(t, mem, scope)
	if out.Failed != 1 {
		t.Fatalf("side-effect processor result = %+v, want failed=1", out)
	}
	got, listErr := store.List(context.Background(), scope, port.ListQuery{})
	if listErr != nil {
		t.Fatalf("store.List: %v", listErr)
	}
	if len(got) != 1 {
		t.Fatalf("side-effect failure must not roll back canonical facts: %+v", got)
	}
}

func TestSave_StateSecondWriteSupersedesPrior(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, _ := New(WithTemporalStore(store))
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
	drainSideEffectsForTest(t, mem, scope)
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
	mem, _ := New(WithTemporalStore(store))
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
	mem, _ := New(WithTemporalStore(store))
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
	mem, _ := New(WithTemporalStore(store))
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

	memA, _ := New(WithTemporalStore(store))
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
	memB, _ := New(WithTemporalStore(store))

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
	mem, _ := New(WithTemporalStore(store))
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
	mem, _ := New(WithTemporalStore(store), withCompiler(cp))

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
	mem, _ := New(WithTemporalStore(store), withCompiler(cp))

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

func TestSave_ProjectionFailureAfterSupersedeDoesNotRollbackCanonical(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	proj := &failOnProjectN{failOn: 2}
	mem, err := New(WithTemporalStore(store), withExtraProjection(proj))
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
	drainSideEffectsForTest(t, mem, scope)
	priorID := first.FactIDs[0]

	second, err := mem.Save(context.Background(), scope, SaveRequest{
		Facts: []TemporalFact{{
			Kind: FactState, Subject: "alice", Predicate: "city",
			Content: "Berlin",
		}},
	})
	if err != nil {
		t.Fatalf("second save should commit canonical state before side effects: %v", err)
	}
	out := drainSideEffectsForTest(t, mem, scope)
	if out.Failed != 1 {
		t.Fatalf("side-effect result = %+v, want failed=1", out)
	}

	prior, err := store.Get(context.Background(), scope, priorID)
	if err != nil {
		t.Fatalf("prior fact should still exist: %v", err)
	}
	if prior.CorrectedBy != second.FactIDs[0] || prior.ValidTo == nil {
		t.Fatalf("side-effect failure must not reopen prior canonical fact, got %+v", prior)
	}
	list, err := store.List(context.Background(), scope, port.ListQuery{IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("side-effect failure must not roll back new canonical fact, got %+v", list)
	}
}

func TestSave_CrossAgentSameMergeKeyDoesNotSupersedeOtherAgentFact(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	mem, err := New(WithTemporalStore(store))
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
