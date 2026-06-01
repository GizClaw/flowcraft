package claw

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestContextStateRoundTrip(t *testing.T) {
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app, err := New(ws, WithConfig(defaultConfig()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	board := engine.NewBoard()
	board.SetVar("current_arc", "arc_03_pilgrimage")
	board.SetVar("story_state", map[string]any{"current_arc": "arc_03_pilgrimage"})
	board.SetVar("response", "transient answer")
	board.SetVar("__usage", map[string]any{"total": 10})

	ctx := context.Background()
	if err := app.saveContextState(ctx, "ctx/story", &agent.Result{RunID: "run-1", LastBoard: board}); err != nil {
		t.Fatalf("saveContextState: %v", err)
	}
	st, err := app.loadContextState(ctx, "ctx/story")
	if err != nil {
		t.Fatalf("loadContextState: %v", err)
	}
	if st.LastRunID != "run-1" {
		t.Fatalf("LastRunID = %q, want run-1", st.LastRunID)
	}
	if st.Vars["current_arc"] != "arc_03_pilgrimage" {
		t.Fatalf("current_arc = %v", st.Vars["current_arc"])
	}
	if _, ok := st.Vars["response"]; ok {
		t.Fatal("response should not be persisted")
	}
	if _, ok := st.Vars["__usage"]; ok {
		t.Fatal("__usage should not be persisted")
	}
}
