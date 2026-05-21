package stages_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
)

func TestParseCanonicalTurns_RoundTrip(t *testing.T) {
	content := "Alice: hello\nBob: world"
	got := stages.ParseCanonicalTurns(content)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Speaker != "Alice" || got[0].Text != "hello" {
		t.Errorf("turn0 = %+v", got[0])
	}
	if got[1].Speaker != "Bob" || got[1].Text != "world" {
		t.Errorf("turn1 = %+v", got[1])
	}
}

func TestReconstructTurnsForJob_PrefersTurnsSnapshot(t *testing.T) {
	store := temporalstore.NewMemoryStore()
	ctx := context.Background()
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	ep := domain.TemporalFact{
		ID:      "ep-1",
		Kind:    domain.KindEpisode,
		Scope:   scope,
		Content: "Alice: canonical-only",
	}
	if err := store.Append(ctx, []domain.TemporalFact{ep}); err != nil {
		t.Fatal(err)
	}
	job := port.AsyncSemanticJob{
		Scope:          scope,
		EpisodeFactIDs: []string{"ep-1"},
		TurnsSnapshot: []domain.TurnContext{{
			ID:      "turn-stable",
			Speaker: "Bob",
			Text:    "from snapshot",
		}},
	}
	turns, err := stages.ReconstructTurnsForJob(ctx, store, job)
	if err != nil {
		t.Fatalf("ReconstructTurnsForJob: %v", err)
	}
	if len(turns) != 1 || turns[0].ID != "turn-stable" {
		t.Fatalf("turns = %+v, want snapshot turn with stable ID", turns)
	}
}
