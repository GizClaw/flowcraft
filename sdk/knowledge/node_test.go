package knowledge_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
)

func newNodeBoardCtx(t *testing.T) (graph.ExecutionContext, *graph.Board) {
	t.Helper()
	board := graph.NewBoard()
	ectx := graph.ExecutionContext{Context: context.Background()}
	return ectx, board
}

func TestKnowledgeServiceNode_NilService_ReturnsEmpty(t *testing.T) {
	n := knowledge.NewKnowledgeServiceNode("k", nil, knowledge.KnowledgeNodeConfig{})
	ectx, board := newNodeBoardCtx(t)
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	if h, ok := hits.([]knowledge.Hit); !ok || len(h) != 0 {
		t.Fatalf("hits = %v, want []Hit{}", hits)
	}
	results, _ := board.GetVar("results")
	if r, ok := results.([]map[string]any); !ok || len(r) != 0 {
		t.Fatalf("results = %v, want []map{}", results)
	}
}

func TestKnowledgeServiceNode_AllScope_FansOut(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "ds1", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put ds1: %v", err)
	}
	if err := svc.PutDocument(ctx, "ds2", "b.md", "alpha gamma"); err != nil {
		t.Fatalf("put ds2: %v", err)
	}

	n := knowledge.NewKnowledgeServiceNode("k", svc, knowledge.KnowledgeNodeConfig{
		Scope: knowledge.ScopeAllDatasets,
		Mode:  knowledge.ModeBM25,
		TopK:  10,
	})
	ectx, board := newNodeBoardCtx(t)
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	got, ok := hits.([]knowledge.Hit)
	if !ok {
		t.Fatalf("hits type = %T, want []knowledge.Hit", hits)
	}
	if len(got) < 2 {
		t.Fatalf("hits = %d, want >=2 (one per dataset)", len(got))
	}
	// legacy "results" projection must also be populated.
	if r, _ := board.GetVar("results"); r == nil {
		t.Fatalf("legacy results not set")
	}
}

func TestKnowledgeServiceNode_SingleScope_PerDatasetStateKey(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "docs", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}

	n := knowledge.NewKnowledgeServiceNode("k", svc, knowledge.KnowledgeNodeConfig{
		Scope: knowledge.ScopeSingleDataset,
		Mode:  knowledge.ModeBM25,
		Datasets: []knowledge.DatasetQuery{
			{DatasetID: "docs", StateKey: "docsHits", TopK: 5},
		},
	})
	ectx, board := newNodeBoardCtx(t)
	board.SetVar("query", "alpha")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	stateHits, ok := board.GetVar("docsHits")
	if !ok {
		t.Fatalf("state key not populated")
	}
	if h, ok := stateHits.([]knowledge.Hit); !ok || len(h) == 0 {
		t.Fatalf("stateHits = %v, want non-empty []Hit", stateHits)
	}
	byDataset, _ := board.GetVar("by_dataset")
	bd, ok := byDataset.(map[string][]knowledge.Hit)
	if !ok {
		t.Fatalf("by_dataset type = %T", byDataset)
	}
	if _, ok := bd["docs"]; !ok {
		t.Fatalf("by_dataset missing docs entry: %v", bd)
	}
}

func TestKnowledgeServiceNode_SingleScope_FallsBackToBoardDataset(t *testing.T) {
	svc := newLocalService(t)
	ctx := context.Background()
	if err := svc.PutDocument(ctx, "docs", "a.md", "alpha banana"); err != nil {
		t.Fatalf("put: %v", err)
	}
	n := knowledge.NewKnowledgeServiceNode("k", svc, knowledge.KnowledgeNodeConfig{
		Scope: knowledge.ScopeSingleDataset,
		Mode:  knowledge.ModeBM25,
		TopK:  5,
	})
	ectx, board := newNodeBoardCtx(t)
	board.SetVar("query", "alpha")
	board.SetVar("dataset_id", "docs")
	if err := n.ExecuteBoard(ectx, board); err != nil {
		t.Fatalf("execute: %v", err)
	}
	hits, _ := board.GetVar("hits")
	if h, ok := hits.([]knowledge.Hit); !ok || len(h) == 0 {
		t.Fatalf("hits = %v, want non-empty", hits)
	}
}

func TestKnowledgeNodeConfigFromMap_LegacyMaxLayer(t *testing.T) {
	cfg := knowledge.KnowledgeNodeConfigFromMap(map[string]any{
		"max_layer": "L1",
		"top_k":     float64(7),
	})
	if cfg.Layer != knowledge.LayerOverview {
		t.Fatalf("Layer = %q, want %q", cfg.Layer, knowledge.LayerOverview)
	}
	if cfg.TopK != 7 {
		t.Fatalf("TopK = %d, want 7", cfg.TopK)
	}
}

func TestKnowledgeNodeConfigFromMap_PrefersLayerOverMaxLayer(t *testing.T) {
	cfg := knowledge.KnowledgeNodeConfigFromMap(map[string]any{
		"layer":     "L0",
		"max_layer": "L1",
	})
	if cfg.Layer != knowledge.LayerAbstract {
		t.Fatalf("Layer = %q, want %q", cfg.Layer, knowledge.LayerAbstract)
	}
}

func TestKnowledgeNodeConfigFromMap_AllScopeAndDatasets(t *testing.T) {
	cfg := knowledge.KnowledgeNodeConfigFromMap(map[string]any{
		"scope": "all",
		"mode":  "hybrid",
		"datasets": []any{
			map[string]any{"dataset_id": "docs", "state_key": "out", "top_k": float64(3)},
		},
	})
	if cfg.Scope != knowledge.ScopeAllDatasets {
		t.Fatalf("Scope = %v, want all", cfg.Scope)
	}
	if cfg.Mode != knowledge.ModeHybrid {
		t.Fatalf("Mode = %q, want hybrid", cfg.Mode)
	}
	if len(cfg.Datasets) != 1 || cfg.Datasets[0].DatasetID != "docs" || cfg.Datasets[0].TopK != 3 {
		t.Fatalf("Datasets = %+v", cfg.Datasets)
	}
}
