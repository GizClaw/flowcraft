package retrieval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/projection/entity"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/memory/storage"
	documentview "github.com/GizClaw/flowcraft/memory/views/document"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type providerSearcher func(context.Context, component.SearchRequest) ([]component.Candidate, error)

func (searcher providerSearcher) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	return searcher(ctx, request)
}

type providerReranker func(context.Context, component.RerankRequest) ([]component.Candidate, error)

func (reranker providerReranker) Rerank(ctx context.Context, request component.RerankRequest) ([]component.Candidate, error) {
	return reranker(ctx, request)
}

type recallRecorder struct{ events []sdkmemory.RecallEvent }

func (recorder *recallRecorder) RecordRecall(_ context.Context, event sdkmemory.RecallEvent) error {
	recorder.events = append(recorder.events, event)
	return nil
}

type hiddenVisibility struct{}

func (hiddenVisibility) Visible(context.Context, sdkmemory.Scope, string) (bool, error) {
	return false, nil
}

func TestProviderReinforcesOnlyActuallyReturnedLongTermItems(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, workspace.NewMemWorkspace())
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	for _, id := range []string{"a", "b"} {
		_, _ = facts.Add(ctx, factview.AddRequest{ID: id, Scope: scope, ConversationID: "conversation",
			Content: providerText(id), Provenance: []sdkmemory.SourceRef{source}})
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("a", 1), providerCandidate("b", .5)}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	recorder := &recallRecorder{}
	now := time.Date(2026, 8, 5, 5, 0, 0, 0, time.UTC)
	provider, _ := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		RecallEvents: recorder, Clock: func() time.Time { return now },
	})
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100}, RecallEventID: "invocation",
	})
	if err != nil || len(result.Items) != 1 || len(recorder.events) != 1 ||
		len(recorder.events[0].ItemIDs) != 1 || recorder.events[0].ItemIDs[0] != result.Items[0].Identity(scope) {
		t.Fatalf("result=%#v events=%#v err=%v", result, recorder.events, err)
	}
	_, err = provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil || len(recorder.events) != 1 {
		t.Fatalf("missing event id reinforced: %#v, %v", recorder.events, err)
	}
}

func TestProviderKeepsEqualLocalFactIDsAcrossConversations(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, workspace.NewMemWorkspace())
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	for _, conversationID := range []string{"conversation-a", "conversation-b"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: "same-fact", Scope: scope, ConversationID: conversationID,
			Content: providerText(conversationID), Provenance: []sdkmemory.SourceRef{source},
		}); err != nil {
			t.Fatal(err)
		}
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		result := make([]component.Candidate, 0, 2)
		for _, conversationID := range []string{"conversation-a", "conversation-b"} {
			result = append(result, component.Candidate{
				ID: "projection-" + conversationID, Lane: "vector", Name: "fact", Score: 1, Source: source,
				Address: component.CandidateAddress{
					Kind: sdkmemory.ContextFact, ConversationID: conversationID, ItemID: "same-fact",
				},
			})
		}
		return result, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	recorder := &recallRecorder{}
	provider, _ := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		RecallEvents: recorder,
	})
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, Query: "same", Budget: sdkmemory.Budget{MaxItems: 2, MaxTokens: 100}, RecallEventID: "recall",
	})
	if err != nil || len(result.Items) != 2 || len(recorder.events) != 1 || len(recorder.events[0].ItemIDs) != 2 {
		t.Fatalf("qualified result=%#v events=%#v err=%v", result, recorder.events, err)
	}
	if result.Items[0].Identity(scope) == result.Items[1].Identity(scope) {
		t.Fatal("conversation-qualified facts shared lifecycle identity")
	}
}

func TestProviderHonorsExplicitSoftForgottenOverlay(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, workspace.NewMemWorkspace())
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	_, _ = facts.Add(ctx, factview.AddRequest{ID: "hidden", Scope: scope, ConversationID: "conversation",
		Content: providerText("hidden"), Provenance: []sdkmemory.SourceRef{source}})
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("hidden", 1)}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	provider, _ := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil), Visibility: hiddenVisibility{},
	})
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "hidden",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 0 {
		t.Fatalf("soft-forgotten result = %#v, %v", result, err)
	}
}

func TestContextProviderProgressivelyHydratesDocumentParents(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	documents, _ := documentview.NewDocumentViewStore(kvStore)
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceDocument, ID: "dataset/document", Revision: "1"}
	metadata := sdkmemory.Metadata{"dataset_id": "dataset", "document_id": "document"}
	records := []documentview.Chunk{
		{ID: "resource", Kind: documentview.KindResource, Level: 0, Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("resource"), Provenance: []sdkmemory.SourceRef{source}, Metadata: metadata},
		{ID: "section", Kind: documentview.KindSection, Level: 1, ParentID: "resource", Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("section"), Provenance: []sdkmemory.SourceRef{source}, Metadata: metadata},
		{ID: "chunk", Kind: documentview.KindChunk, Level: 2, ParentID: "section", Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("chunk"), Provenance: []sdkmemory.SourceRef{source}, Metadata: metadata},
	}
	if _, err := documents.ReplaceDocument(ctx, documentview.ReplaceRequest{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Chunks: records,
	}); err != nil {
		t.Fatal(err)
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: "chunk", Lane: "vector", Name: "chunk", Score: 1, Source: source,
			Address: component.CandidateAddress{
				Kind: sdkmemory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk",
			},
		}}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}}})
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Chunks: documents}, Packer: pack.New(nil), ExpandParents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, Query: "chunk", Budget: sdkmemory.Budget{MaxItems: 3, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 3 || result.Items[0].ID != "chunk" ||
		result.Items[1].ID != "section" || result.Items[2].ID != "resource" {
		t.Fatalf("progressive result = %#v, %v", result, err)
	}
}

func TestContextProviderIntegrationDegradesHydratesFiltersAndPacks(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	facts := newFactStore(t, workspace.NewMemWorkspace())
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	for _, fact := range []struct{ id, text string }{{"high", "important fact"}, {"low", "less relevant"}} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: fact.id, Scope: scope, ConversationID: "conversation",
			Content: providerText(fact.text), Provenance: []sdkmemory.SourceRef{source},
		}); err != nil {
			t.Fatal(err)
		}
	}
	good := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("high", 10), providerCandidate("low", 1)}, nil
	})
	bad := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("entity unavailable")
	})
	fusor, err := fusion.New([]fusion.Lane{
		{Name: "vector", Searcher: good, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "bm25", Searcher: good, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "entity", Searcher: bad, Weight: 1, Calibrator: fusion.MinMax{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(fusor, &hydrate.Composite{Facts: facts}, pack.New(nil))
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "important",
		MinScore: 0.5, Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "high" || result.Items[0].Content.Text() != "important fact" {
		t.Fatalf("result = %+v", result)
	}
	diagnostics := provider.LastDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].Lane != "entity" || len(result.Items[0].Metadata) != 0 {
		t.Fatalf("diagnostics = %+v, item = %+v", diagnostics, result.Items[0])
	}
}

func TestProviderRerankerRejectsInjectionAndDegradesOnFailure(t *testing.T) {
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("original", .8)}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{},
	}})
	for _, reranker := range []providerReranker{
		func(context.Context, component.RerankRequest) ([]component.Candidate, error) {
			return []component.Candidate{providerCandidate("injected", 1)}, nil
		},
		func(context.Context, component.RerankRequest) ([]component.Candidate, error) {
			return nil, errors.New("reranker offline")
		},
		func(_ context.Context, request component.RerankRequest) ([]component.Candidate, error) {
			return []component.Candidate{request.Candidates[0], request.Candidates[0]}, nil
		},
	} {
		provider, err := NewProviderWithConfig(ProviderConfig{
			Fusion: fusor, Hydrator: &hydrate.Composite{}, Packer: pack.New(nil),
			Reranker: RerankerConfig{Enabled: true, Value: reranker},
		})
		if err != nil {
			t.Fatal(err)
		}
		candidates, diagnostics, _, err := provider.readCandidates(context.Background(), sdkmemory.ContextRequest{
			Scope: sdkmemory.Scope{RuntimeID: "runtime"}, Query: "query",
		}, nil, 10)
		if err != nil || len(candidates) != 1 || candidates[0].ID != "original" ||
			len(diagnostics) != 1 || diagnostics[0].Stage != "rerank" {
			t.Fatalf("fallback = %+v, diagnostics=%+v, err=%v", candidates, diagnostics, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	canceling := providerReranker(func(context.Context, component.RerankRequest) ([]component.Candidate, error) {
		cancel()
		return nil, context.Canceled
	})
	provider, _ := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{}, Packer: pack.New(nil),
		Reranker: RerankerConfig{Enabled: true, Value: canceling},
	})
	_, _, _, err := provider.readCandidates(ctx, sdkmemory.ContextRequest{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, Query: "query",
	}, nil, 10)
	if !sdkmemory.IsKind(err, sdkmemory.KindOperationInterrupted) {
		t.Fatalf("reranker cancellation = %v", err)
	}
}

func TestFactEntityProjectionProviderIntegration(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	ws := workspace.NewMemWorkspace()
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts := newFactStore(t, ws)
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "conversation/message"}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content: providerText("Sam Altman leads OpenAI"), Entities: []string{"OpenAI", "Sam Altman"},
		Provenance: []sdkmemory.SourceRef{source},
	}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := facts.Get(ctx, scope, "conversation", "fact")
	if err != nil || !found {
		t.Fatalf("stored fact = %+v, %v", stored, err)
	}
	index, _ := entity.New(entity.Config{KV: kvStore, Projection: "facts"})
	if err := index.ApplyDelta(ctx, component.ProjectionDelta{
		Scope: scope, Upserts: []component.Artifact{{
			Kind: "fact", ID: "fact", Content: stored.Content, Entities: stored.Entities,
			Sources: stored.Provenance, Metadata: sdkmemory.Metadata{
				"context_kind": string(sdkmemory.ContextFact), "conversation_id": "conversation", "item_id": "fact",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "entity", Searcher: index, Weight: 1, Calibrator: fusion.Identity{},
	}})
	provider, _ := NewProvider(fusor, &hydrate.Composite{Facts: facts}, pack.New(nil))
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "What did Sam Altman say?",
		Budget: sdkmemory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "fact" {
		t.Fatalf("entity integration = %+v, %v", result, err)
	}
}

func TestProviderMinScoreStableAcrossCandidateSets(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, workspace.NewMemWorkspace())
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	for _, id := range []string{"target", "distractor"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: id, Scope: scope, ConversationID: "conversation",
			Content: providerText(id), Provenance: []sdkmemory.SourceRef{source},
		}); err != nil {
			t.Fatal(err)
		}
	}
	search := providerSearcher(func(_ context.Context, request component.SearchRequest) ([]component.Candidate, error) {
		result := []component.Candidate{providerCandidate("target", .6)}
		if request.Query == "larger" {
			result = append(result, providerCandidate("distractor", .9))
		}
		return result, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{},
	}})
	provider, _ := NewProvider(fusor, &hydrate.Composite{Facts: facts}, pack.New(nil))
	for _, query := range []string{"smaller", "larger"} {
		result, err := provider.Context(ctx, sdkmemory.ContextRequest{
			Scope: scope, ConversationID: "conversation", Query: query, MinScore: .5,
			Budget: sdkmemory.Budget{MaxItems: 5, MaxTokens: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range result.Items {
			found = found || item.ID == "target"
		}
		if !found {
			t.Fatalf("target crossed MinScore for query %q: %+v", query, result)
		}
	}
}

func TestProviderAllLanesFailedReturnsEmptyDiagnostics(t *testing.T) {
	failing := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("offline")
	})
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "vector", Searcher: failing, Weight: 1, Calibrator: fusion.Cosine{},
	}})
	provider, _ := NewProvider(fusor, &hydrate.Composite{}, pack.New(nil))
	result, err := provider.Context(context.Background(), sdkmemory.ContextRequest{
		Scope: sdkmemory.Scope{RuntimeID: "runtime"}, Query: "query",
		Budget: sdkmemory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	diagnostics := provider.LastDiagnostics()
	if err != nil || len(result.Items) != 0 || len(diagnostics) != 1 || diagnostics[0].Lane != "vector" {
		t.Fatalf("all failed = %+v, diagnostics=%+v, err=%v", result, diagnostics, err)
	}
}

func providerCandidate(id string, score float64) component.Candidate {
	return component.Candidate{
		ID: id, Lane: "native", Name: "fact", Score: score,
		Source: sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"},
		Address: component.CandidateAddress{
			Kind: sdkmemory.ContextFact, ConversationID: "conversation", ItemID: id,
		},
	}
}

func providerText(value string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: value}}}
}
