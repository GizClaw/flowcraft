package stages_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/revision"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/revision/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func seedFact(t *testing.T, store *temporal.MemoryStore, f domain.TemporalFact) {
	t.Helper()
	if f.Scope.RuntimeID == "" {
		f.Scope = domain.Scope{RuntimeID: "rt", UserID: "u"}
	}
	if f.Kind == "" {
		f.Kind = domain.KindNote
	}
	if f.MergeKey == "" {
		f.MergeKey = f.ID + "|k"
	}
	if f.ObservedAt.IsZero() {
		f.ObservedAt = time.Unix(1, 0)
	}
	if err := store.Append(context.Background(), []domain.TemporalFact{f}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestLookupSource_HappyPathPopulatesState verifies the canonical
// flow: the stage fetches the source fact and writes it into
// state.Source so attach_revision can derive merge keys / metadata
// from it without re-querying the store.
func TestLookupSource_HappyPathPopulatesState(t *testing.T) {
	store := temporal.NewMemoryStore()
	seedFact(t, store, domain.TemporalFact{ID: "src", MergeKey: "alice|city"})
	stage := stages.NewLookupSource(store)
	state := &revision.State{
		Scope:        domain.Scope{RuntimeID: "rt", UserID: "u"},
		Mode:         revision.ModeFork,
		SourceFactID: "src",
	}
	if _, err := stage.Run(context.Background(), state); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state.Source.ID != "src" {
		t.Errorf("Source.ID = %q, want src", state.Source.ID)
	}
}

// TestLookupSource_MissingSourceErrors pins that a non-existent
// source id surfaces as an error — Fork / Contest can never silently
// anchor on a phantom fact.
func TestLookupSource_MissingSourceErrors(t *testing.T) {
	store := temporal.NewMemoryStore()
	stage := stages.NewLookupSource(store)
	state := &revision.State{
		Scope:        domain.Scope{RuntimeID: "rt", UserID: "u"},
		Mode:         revision.ModeFork,
		SourceFactID: "ghost",
	}
	if _, err := stage.Run(context.Background(), state); err == nil {
		t.Fatal("expected lookup error for missing source")
	}
}

// TestLookupSource_ForkRejectsClosedSource pins that ModeFork
// refuses to branch off a retired (Closed) fact — forking stale
// state is exactly the bug the validation guards against.
func TestLookupSource_ForkRejectsClosedSource(t *testing.T) {
	store := temporal.NewMemoryStore()
	seedFact(t, store, domain.TemporalFact{ID: "src", Closed: true})
	stage := stages.NewLookupSource(store)
	state := &revision.State{
		Scope:        domain.Scope{RuntimeID: "rt", UserID: "u"},
		Mode:         revision.ModeFork,
		SourceFactID: "src",
	}
	if _, err := stage.Run(context.Background(), state); err == nil {
		t.Fatal("expected fork on retired source to error")
	}
}
