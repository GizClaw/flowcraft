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
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, nil)
	defer app.Close()

	board := engine.NewBoard()
	board.SetVar("current_chapter", 27)
	board.SetVar("tmp_current_arc", "arc_04_trials")
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
	if st.Vars["current_chapter"] != float64(27) {
		t.Fatalf("current_chapter = %v", st.Vars["current_chapter"])
	}
	if _, ok := st.Vars["tmp_current_arc"]; ok {
		t.Fatal("tmp_current_arc should not be persisted")
	}
	if _, ok := st.Vars["response"]; ok {
		t.Fatal("response should not be persisted")
	}
	if _, ok := st.Vars["__usage"]; ok {
		t.Fatal("__usage should not be persisted")
	}
}

func TestMemoryWorkspaceLayout(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.NewLocalWorkspace(root)
	if err != nil {
		t.Fatalf("NewLocalWorkspace: %v", err)
	}
	app := newTestClaw(t, ws, staticLLM{reply: "ok"}, func(cfg *Config) {
		cfg.Memory.Enabled = true
		cfg.Memory.Retrieval.Backend = "bbh"
	})
	defer app.Close()

	for _, path := range []string{
		"memory/metadata",
		"memory/retrieval/badger",
		"memory/retrieval/bleve",
		"memory/retrieval/hnsw",
	} {
		ok, err := ws.Exists(context.Background(), path)
		if err != nil {
			t.Fatalf("Exists %s: %v", path, err)
		}
		if !ok {
			t.Fatalf("missing %s", path)
		}
	}
}
