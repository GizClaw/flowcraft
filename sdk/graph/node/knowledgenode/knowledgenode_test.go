package knowledgenode_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/knowledgenode"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/knowledge/factory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newLocalService(t *testing.T) *knowledge.Service {
	t.Helper()
	return factory.NewLocal(workspace.NewMemWorkspace())
}

func newNodeBoardCtx() (graph.ExecutionContext, *graph.Board) {
	board := graph.NewBoard()
	return graph.ExecutionContext{Context: context.Background()}, board
}

func TestNode_NilService_ReturnsEmpty(t *testing.T) {
	n := knowledgenode.New("k", nil, knowledgenode.Config{})
	ectx, board := newNodeBoardCtx()
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	if h, ok := hits.([]knowledge.Hit); !ok || len(h) != 0 {
		t.Fatalf("hits = %v, want []Hit{}", hits)
	}
	if r, _ := board.GetVar("results"); r == nil {
		t.Fatal("compat results projection not set")
	}
}

func TestNode_AllScope_FansOut(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds1", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put ds1: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds2", "b.md", "alpha gamma"); err != nil {
		t.Fatalf("put ds2: %v", err)
	}

	n := knowledgenode.New("k", svc, knowledgenode.Config{
		Scope: knowledge.ScopeAllDatasets,
		Mode:  knowledge.ModeBM25,
		TopK:  10,
	})
	ectx, board := newNodeBoardCtx()
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	got, ok := hits.([]knowledge.Hit)
	if !ok || len(got) < 2 {
		t.Fatalf("hits = %v, want >=2 entries", hits)
	}
}

func TestNode_SingleScope_PerDatasetStateKey(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "docs", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}

	n := knowledgenode.New("k", svc, knowledgenode.Config{
		Scope: knowledge.ScopeSingleDataset,
		Mode:  knowledge.ModeBM25,
		Datasets: []knowledge.DatasetQuery{
			{DatasetID: "docs", StateKey: "docsHits", TopK: 5},
		},
	})
	ectx, board := newNodeBoardCtx()
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	stateHits, ok := board.GetVar("docsHits")
	if !ok {
		t.Fatal("state key not populated")
	}
	if h, ok := stateHits.([]knowledge.Hit); !ok || len(h) == 0 {
		t.Fatalf("stateHits = %v", stateHits)
	}
	byDataset, _ := board.GetVar("by_dataset")
	if _, ok := byDataset.(map[string][]knowledge.Hit)["docs"]; !ok {
		t.Fatalf("by_dataset missing docs entry: %v", byDataset)
	}
}

func TestNode_SingleScope_FallsBackToBoardDataset(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "docs", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}
	n := knowledgenode.New("k", svc, knowledgenode.Config{
		Scope: knowledge.ScopeSingleDataset,
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	ectx, board := newNodeBoardCtx()
	board.SetVar("query", "alpha")
	board.SetVar("dataset_id", "docs")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	if h, ok := hits.([]knowledge.Hit); !ok || len(h) == 0 {
		t.Fatalf("hits = %v", hits)
	}
}

func TestConfigFromMap_LegacyMaxLayer(t *testing.T) {
	cfg := knowledgenode.ConfigFromMap(map[string]any{
		"max_layer": "L1",
		"top_k":     float64(7),
	})
	if cfg.Layer != knowledge.LayerOverview {
		t.Fatalf("Layer = %q", cfg.Layer)
	}
	if cfg.TopK != 7 {
		t.Fatalf("TopK = %d", cfg.TopK)
	}
}

func TestConfigFromMap_PrefersLayerOverMaxLayer(t *testing.T) {
	cfg := knowledgenode.ConfigFromMap(map[string]any{
		"layer":     "L0",
		"max_layer": "L1",
	})
	if cfg.Layer != knowledge.LayerAbstract {
		t.Fatalf("Layer = %q", cfg.Layer)
	}
}

func TestConfigFromMap_AllScopeAndDatasets(t *testing.T) {
	cfg := knowledgenode.ConfigFromMap(map[string]any{
		"scope": "all",
		"mode":  "hybrid",
		"datasets": []any{
			map[string]any{"dataset_id": "docs", "state_key": "out", "top_k": float64(3)},
		},
	})
	if cfg.Scope != knowledge.ScopeAllDatasets {
		t.Fatalf("Scope = %v", cfg.Scope)
	}
	if cfg.Mode != knowledge.ModeHybrid {
		t.Fatalf("Mode = %q", cfg.Mode)
	}
	if len(cfg.Datasets) != 1 || cfg.Datasets[0].DatasetID != "docs" || cfg.Datasets[0].TopK != 3 {
		t.Fatalf("Datasets = %+v", cfg.Datasets)
	}
}

func TestRegister_BuildsKnowledgeNode(t *testing.T) {
	f := node.NewFactory()
	knowledgenode.Register(f, nil) // nil svc is fine — node falls back to empty hits

	n, err := f.Build(graph.NodeDefinition{
		ID:     "k1",
		Type:   "knowledge",
		Config: map[string]any{"top_k": float64(5)},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if n.ID() != "k1" || n.Type() != "knowledge" {
		t.Fatalf("identity mismatch: %q/%q", n.ID(), n.Type())
	}
}
