package config

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestRegistryResolveSearchLanes(t *testing.T) {
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	registry := NewDriverRegistry()
	if err := registry.RegisterSearchDriver("lsm", func(
		deps SearchDriverDeps,
		_ json.RawMessage,
	) (retrieval.SearchBackend, error) {
		return retrieval.NewLaneBackend(deps.Lane), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterSearchDriver("lsm", nil); err == nil {
		t.Fatal("duplicate search driver accepted")
	}
	lane := &fakeSearcher{}
	_, err = registry.ResolveSearchLanes(SearchSettings{
		Lanes: map[string]BackendSettings{
			"vector": {Driver: "lsm"},
			"bm25":   {Driver: "missing"},
		},
	}, map[string]component.Searcher{"vector": lane}, kvStore)
	if err == nil {
		t.Fatal("unknown search driver accepted")
	}
	backends, err := registry.ResolveSearchLanes(SearchSettings{
		Lanes: map[string]BackendSettings{
			"vector": {Driver: "lsm"},
		},
	}, map[string]component.Searcher{"vector": lane}, kvStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 1 {
		t.Fatalf("backends = %d, want 1", len(backends))
	}
	if _, err := backends["vector"].Search(context.Background(), "facts", retrieval.SearchQuery{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"},
		Text:  "alpha",
	}); err != nil {
		t.Fatal(err)
	}
}

type fakeSearcher struct{}

func (fakeSearcher) Search(context.Context, component.SearchRequest) ([]component.Candidate, error) {
	return []component.Candidate{}, nil
}
