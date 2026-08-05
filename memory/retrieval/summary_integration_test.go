package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/memory/derive/summary"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestFactCompactProviderIntegration(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	ws := workspace.NewMemWorkspace()
	facts, _ := factview.NewWorkspaceStore(ws)
	summaries, _ := summaryview.NewWorkspaceStore(ws)
	source := sdkmemory.SourceRef{
		Kind: sdkmemory.SourceMessage, ID: "conversation/message", Revision: "1",
	}
	fact, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:  providerText("prefers deterministic architecture"),
		Entities: []string{"architecture"}, Provenance: []sdkmemory.SourceRef{source},
	})
	if err != nil {
		t.Fatal(err)
	}
	compactor, _ := summaryderive.New(summaryderive.DefaultConfig(), summaries, nil)
	if _, err := compactor.Compact(ctx, summaryderive.CompactRequest{
		Scope: scope, ConversationID: "conversation", GenerationID: "generation",
		Inputs: []summaryderive.Input{{
			ID: fact.ID, Text: fact.Text, Topics: fact.Entities, SourceRefs: fact.Provenance,
			CoverageRange: summaryview.CoverageRange{StartSeq: 1, EndSeq: 1},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	empty := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "empty", Searcher: empty, Weight: 1, Calibrator: fusion.Identity{CalibrationVersion: "identity-v1"},
	}})
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Summary: &summaryview.Searcher{Store: summaries},
		Hydrator: &hydrate.Composite{Summaries: summaries}, Packer: pack.New(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "architecture",
		Budget: sdkmemory.Budget{MaxItems: 4, MaxTokens: 100},
	})
	if err != nil || len(result.Items) == 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	got := result.Items[0]
	if got.Kind != sdkmemory.ContextSummary || got.SourceClass != sdkmemory.ContextSourceSummary ||
		got.Hint == nil || got.Content.Text() != "prefers deterministic architecture" {
		t.Fatalf("summary item=%#v", got)
	}
}
