package retrieval

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	summaryderive "github.com/GizClaw/flowcraft/backends/memory/derive/summary"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/backends/memory/views/summary"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func TestFactCompactProviderIntegration(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	ws := newTestWorkspace(t)
	facts := newFactStore(t, ws)
	summaries := newSummaryStore(t, ws)
	source := corememory.SourceRef{
		Kind: corememory.SourceMessage, ID: "conversation/message", Revision: "1",
	}
	fact, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:  providerText("prefers deterministic architecture"),
		Entities: []string{"architecture"}, Provenance: []corememory.SourceRef{source},
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
	}, {
		Name: "summary", Searcher: &summaryview.Searcher{Store: summaries},
		Weight: 0.5, Calibrator: fusion.Identity{CalibrationVersion: "identity-v1"},
	}})
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion:   fusor,
		Hydrator: &hydrate.Composite{Summaries: summaries}, Packer: pack.New(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "architecture",
		Budget: corememory.Budget{MaxItems: 4, MaxTokens: 100},
	})
	if err != nil || len(result.Items) == 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	got := result.Items[0]
	if got.Kind != corememory.ContextSummary || got.SourceClass != corememory.ContextSourceSummary ||
		got.Content.Text() != "prefers deterministic architecture" {
		t.Fatalf("summary item=%#v", got)
	}
	// The summary lane is fused and calibrated: a threshold above the
	// weighted fused score drops the record, while the old un-fused path
	// would have presented the raw lexical ratio (1.0) and passed.
	if got.Score > 0.5 {
		t.Fatalf("summary score = %v, want the weighted fused score", got.Score)
	}
	filtered, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "architecture",
		MinScore: 0.9, Budget: corememory.Budget{MaxItems: 4, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 0 {
		t.Fatalf("min_score result=%#v", filtered.Items)
	}
}
