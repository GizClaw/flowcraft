package kanban

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// RestoreBoard — rebuild a board from persisted KanbanCardModel rows.
//
// Bug 5 (issue #28): historical CreatedAt/UpdatedAt must survive restore so
// timeline/SLA metrics keep reporting accurate elapsed times.
// ---------------------------------------------------------------------------

func TestRestoreBoard_AllStates(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cards := []*KanbanCardModel{
		{
			ID: "c1", RuntimeID: "s1", Type: "task", Status: "pending",
			Producer: "copilot", Consumer: "*",
			Query: "q1", TargetAgentID: "agent-1",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "c2", RuntimeID: "s1", Type: "task", Status: "claimed",
			Producer: "copilot", Consumer: "agent-2",
			Query: "q2", TargetAgentID: "agent-2",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "c3", RuntimeID: "s1", Type: "task", Status: "done",
			Producer: "copilot", Consumer: "agent-3",
			Query: "q3", TargetAgentID: "agent-3", Output: "result", RunID: "run-1",
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "c4", RuntimeID: "s1", Type: "task", Status: "failed",
			Producer: "copilot", Consumer: "agent-4",
			Query: "q4", TargetAgentID: "agent-4", Error: "timeout",
			CreatedAt: now, UpdatedAt: now,
		},
	}

	tb := RestoreBoard("s1", cards)
	t.Cleanup(tb.Close)

	if tb.ScopeID() != "s1" {
		t.Fatalf("ScopeID = %q, want s1", tb.ScopeID())
	}

	got := make(map[string]string)
	for _, c := range tb.Cards() {
		got[c.ID] = c.Status
	}
	want := map[string]string{
		"c1": "pending", "c2": "claimed", "c3": "done", "c4": "failed",
	}
	if len(got) != len(want) {
		t.Fatalf("Cards()=%d, want %d", len(got), len(want))
	}
	for id, status := range want {
		if got[id] != status {
			t.Errorf("card %s: status=%q, want %q", id, got[id], status)
		}
	}

	// Done card retains output / run_id during restore.
	for _, c := range tb.Cards() {
		if c.ID != "c3" {
			continue
		}
		if c.RunID != "run-1" || c.Output != "result" {
			t.Errorf("c3 lost done-state fields: %+v", c)
		}
	}
}

func TestRestoreBoard_Empty(t *testing.T) {
	t.Parallel()
	tb := RestoreBoard("s-empty", nil)
	t.Cleanup(tb.Close)

	if tb.ScopeID() != "s-empty" {
		t.Fatalf("ScopeID = %q, want s-empty", tb.ScopeID())
	}
	if got := len(tb.Cards()); got != 0 {
		t.Fatalf("Cards()=%d, want 0", got)
	}
}

// Bug 5: historical timestamps must not be replaced by time.Now() during
// the synthetic Claim/Done/Fail calls RestoreBoard makes internally.
func TestRestoreBoard_PreservesTimestamps(t *testing.T) {
	t.Parallel()
	created := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2024, 1, 2, 10, 5, 30, 0, time.UTC)

	tb := RestoreBoard("s1", []*KanbanCardModel{{
		ID: "c-old", RuntimeID: "s1", Type: "task", Status: "done",
		Producer: "copilot", Consumer: "agent-1",
		Query: "q", TargetAgentID: "agent-1", Output: "ok",
		CreatedAt: created, UpdatedAt: updated,
	}})
	t.Cleanup(tb.Close)

	got, ok := tb.GetCardByID("c-old")
	if !ok {
		t.Fatal("restored card not found")
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt overwritten: got %v, want %v", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt overwritten: got %v, want %v", got.UpdatedAt, updated)
	}

	// Derived fields (e.g. ElapsedMs) must reflect the restored stamps.
	infos := tb.Cards()
	if len(infos) != 1 {
		t.Fatalf("Cards()=%d, want 1", len(infos))
	}
	wantElapsed := updated.Sub(created).Milliseconds()
	if infos[0].ElapsedMs != wantElapsed {
		t.Errorf("ElapsedMs = %d, want %d", infos[0].ElapsedMs, wantElapsed)
	}
}

// WithTimestamps is the public hook RestoreBoard now uses internally.
// Verify it overrides Produce's default time.Now() but does not break the
// default path when not supplied.
func TestBoard_WithTimestamps(t *testing.T) {
	t.Parallel()
	b := newBoard(t)

	created := time.Date(2023, 6, 1, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2023, 6, 1, 12, 30, 0, 0, time.UTC)

	t.Run("override", func(t *testing.T) {
		card := b.Produce("task", "p", nil, WithTimestamps(created, updated))
		if !card.CreatedAt.Equal(created) {
			t.Errorf("CreatedAt = %v, want %v", card.CreatedAt, created)
		}
		if !card.UpdatedAt.Equal(updated) {
			t.Errorf("UpdatedAt = %v, want %v", card.UpdatedAt, updated)
		}
	})

	t.Run("default_still_now", func(t *testing.T) {
		card := b.Produce("task", "p", nil)
		if card.CreatedAt.IsZero() {
			t.Fatal("default Produce must stamp CreatedAt")
		}
		if time.Since(card.CreatedAt) > time.Second {
			t.Fatalf("default CreatedAt should be ~now, got %v ago", time.Since(card.CreatedAt))
		}
	})
}
