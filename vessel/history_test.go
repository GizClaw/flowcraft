package vessel

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// recordingEngine returns the entire MainChannel as the assistant
// reply (joined by "|"), so tests can assert on what the seeder
// pre-loaded onto the board.
func recordingEngine() engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		main := b.Channel(engine.MainChannel)
		var s string
		for i, m := range main {
			if i > 0 {
				s += "|"
			}
			s += m.Content()
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, s))
		return b, nil
	})
}

// TestHistory_ReadWrite_RoundTrips verifies the seeder + appender
// pair: a second turn against the same ContextID sees the first
// turn's assistant reply already loaded onto the board.
func TestHistory_ReadWrite_RoundTrips(t *testing.T) {
	t.Parallel()
	vs := spec.Spec{
		Agents:  []spec.Agent{{Name: "p"}},
		History: &spec.History{Kind: "buffer"},
	}
	c, err := New(vs, WithEngine(recordingEngine()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	r1, err := c.Call(context.Background(), "p", agent.Request{
		ContextID: "ctx-1",
		Message:   model.NewTextMessage(model.RoleUser, "first"),
	})
	if err != nil || r1.Text() != "first" {
		t.Fatalf("turn 1: %+v / %v", r1, err)
	}

	r2, err := c.Call(context.Background(), "p", agent.Request{
		ContextID: "ctx-1",
		Message:   model.NewTextMessage(model.RoleUser, "second"),
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	// Expect the seeded board to contain: "first" (user) | "first"
	// (assistant from turn 1) | "second" (user) — the engine
	// concatenates them, so the assistant reply is the joined
	// string.
	if got := r2.Text(); got != "first|first|second" {
		t.Fatalf("turn 2 text = %q, want first|first|second", got)
	}
}

// TestHistory_ReadOnly_DoesNotAppend confirms a HistoryAccessReadOnly
// agent observes prior turns but its own output is NOT persisted.
func TestHistory_ReadOnly_DoesNotAppend(t *testing.T) {
	t.Parallel()
	store := history.NewBuffer(history.NewInMemoryStore())
	vs := spec.Spec{
		Agents: []spec.Agent{
			{Name: "moderator", HistoryAccess: spec.HistoryAccessReadOnly},
		},
		History: &spec.History{Kind: "buffer"},
	}
	c, err := New(vs, WithEngine(recordingEngine()), WithHistory(store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	// Pre-seed the store so the moderator sees something.
	if err := store.Append(context.Background(), "ctx-2", []model.Message{
		model.NewTextMessage(model.RoleUser, "pre-existing"),
	}); err != nil {
		t.Fatalf("preseed: %v", err)
	}

	if _, err := c.Call(context.Background(), "moderator", agent.Request{
		ContextID: "ctx-2",
		Message:   model.NewTextMessage(model.RoleUser, "audit"),
	}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	got, err := store.Load(context.Background(), "ctx-2", history.Budget{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Only the pre-existing message should remain — the audit user
	// message and the moderator's reply must NOT have been appended
	// because the moderator is ReadOnly. (The seeder appends the
	// per-turn user message onto the BOARD only, not the store.)
	if len(got) != 1 || got[0].Content() != "pre-existing" {
		t.Fatalf("store after ReadOnly run = %v, want [pre-existing]", got)
	}
}

// TestHistory_None_BypassesStore confirms HistoryAccessNone agents
// neither see the prior transcript nor write back.
func TestHistory_None_BypassesStore(t *testing.T) {
	t.Parallel()
	store := history.NewBuffer(history.NewInMemoryStore())
	if err := store.Append(context.Background(), "ctx-3", []model.Message{
		model.NewTextMessage(model.RoleUser, "earlier"),
	}); err != nil {
		t.Fatalf("preseed: %v", err)
	}

	vs := spec.Spec{
		Agents:  []spec.Agent{{Name: "p", HistoryAccess: spec.HistoryAccessNone}},
		History: &spec.History{Kind: "buffer"},
	}
	c, err := New(vs, WithEngine(recordingEngine()), WithHistory(store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop(context.Background())
	_ = c.Launch(context.Background())

	r, err := c.Call(context.Background(), "p", agent.Request{
		ContextID: "ctx-3",
		Message:   model.NewTextMessage(model.RoleUser, "fresh"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The board only has the per-turn user message — "earlier" was
	// NOT seeded.
	if r.Text() != "fresh" {
		t.Fatalf("text = %q, want fresh", r.Text())
	}
}
