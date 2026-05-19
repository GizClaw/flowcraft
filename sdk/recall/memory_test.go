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

func TestRecall_NotImplementedYet(t *testing.T) {
	mem, _ := New()
	_, err := mem.Recall(context.Background(), Scope{RuntimeID: "rt"}, Query{Text: "x"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
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
