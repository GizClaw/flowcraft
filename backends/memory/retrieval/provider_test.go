package retrieval

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/projection/entity"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	documentview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type providerSearcher func(context.Context, component.SearchRequest) ([]component.Candidate, error)

func (searcher providerSearcher) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	return searcher(ctx, request)
}

type recallRecorder struct{ events []corememory.RecallEvent }

func (recorder *recallRecorder) RecordRecall(_ context.Context, event corememory.RecallEvent) error {
	recorder.events = append(recorder.events, event)
	return nil
}

type hiddenVisibility struct{}

func (hiddenVisibility) Visible(context.Context, corememory.Scope, string) (bool, error) {
	return false, nil
}

func TestProviderReinforcesOnlyActuallyReturnedLongTermItems(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, id := range []string{"a", "b"} {
		_, _ = facts.Add(ctx, factview.AddRequest{ID: id, Scope: scope, ConversationID: "conversation",
			Content: providerText(id), Provenance: []corememory.SourceRef{source}})
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
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: corememory.Budget{MaxItems: 1, MaxTokens: 100}, RecallEventID: "invocation",
	})
	if err != nil || len(result.Items) != 1 || len(recorder.events) != 1 ||
		len(recorder.events[0].ItemIDs) != 1 || recorder.events[0].ItemIDs[0] != result.Items[0].Identity(scope) {
		t.Fatalf("result=%#v events=%#v err=%v", result, recorder.events, err)
	}
	_, err = provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: corememory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil || len(recorder.events) != 1 {
		t.Fatalf("missing event id reinforced: %#v, %v", recorder.events, err)
	}
}

func TestProviderKeepsEqualLocalFactIDsAcrossConversations(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, conversationID := range []string{"conversation-a", "conversation-b"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: "same-fact", Scope: scope, ConversationID: conversationID,
			Content: providerText(conversationID), Provenance: []corememory.SourceRef{source},
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
					Kind: corememory.ContextFact, ConversationID: conversationID, ItemID: "same-fact",
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
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "same", Budget: corememory.Budget{MaxItems: 2, MaxTokens: 100}, RecallEventID: "recall",
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
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	_, _ = facts.Add(ctx, factview.AddRequest{ID: "hidden", Scope: scope, ConversationID: "conversation",
		Content: providerText("hidden"), Provenance: []corememory.SourceRef{source}})
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("hidden", 1)}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	provider, _ := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil), Visibility: hiddenVisibility{},
	})
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "hidden",
		Budget: corememory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 0 {
		t.Fatalf("soft-forgotten result = %#v, %v", result, err)
	}
}

func TestContextProviderProgressivelyHydratesDocumentParents(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	documents, _ := documentview.NewDocumentViewStore(kvStore)
	source := corememory.SourceRef{Kind: corememory.SourceDocument, ID: "dataset/document", Revision: "1"}
	metadata := corememory.Metadata{"dataset_id": "dataset", "document_id": "document"}
	records := []documentview.Chunk{
		{ID: "resource", Kind: documentview.KindResource, Level: 0, Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("resource"), Provenance: []corememory.SourceRef{source}, Metadata: metadata},
		{ID: "section", Kind: documentview.KindSection, Level: 1, ParentID: "resource", Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("section"), Provenance: []corememory.SourceRef{source}, Metadata: metadata},
		{ID: "chunk", Kind: documentview.KindChunk, Level: 2, ParentID: "section", Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("chunk"), Provenance: []corememory.SourceRef{source}, Metadata: metadata},
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
				Kind: corememory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk",
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
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "chunk", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 3 || result.Items[0].ID != "chunk" ||
		result.Items[1].ID != "section" || result.Items[2].ID != "resource" {
		t.Fatalf("progressive result = %#v, %v", result, err)
	}
}

func TestContextProviderIntegrationDegradesHydratesFiltersAndPacks(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, fact := range []struct{ id, text string }{{"high", "important fact"}, {"low", "less relevant"}} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: fact.id, Scope: scope, ConversationID: "conversation",
			Content: providerText(fact.text), Provenance: []corememory.SourceRef{source},
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
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "important",
		MinScore: 0.5, Budget: corememory.Budget{MaxItems: 1, MaxTokens: 100},
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

func TestFactEntityProjectionProviderIntegration(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	ws := newTestWorkspace(t)
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts := newFactStore(t, ws)
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "conversation/message"}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content: providerText("Sam Altman leads OpenAI"), Entities: []string{"OpenAI", "Sam Altman"},
		Provenance: []corememory.SourceRef{source},
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
			Sources: stored.Provenance, Metadata: corememory.Metadata{
				"context_kind": string(corememory.ContextFact), "conversation_id": "conversation", "item_id": "fact",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	fusor, _ := fusion.New([]fusion.Lane{{
		Name: "entity", Searcher: index, Weight: 1, Calibrator: fusion.Identity{},
	}})
	provider, _ := NewProvider(fusor, &hydrate.Composite{Facts: facts}, pack.New(nil))
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "What did Sam Altman say?",
		Budget: corememory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "fact" {
		t.Fatalf("entity integration = %+v, %v", result, err)
	}
}

func TestProviderMinScoreStableAcrossCandidateSets(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, id := range []string{"target", "distractor"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: id, Scope: scope, ConversationID: "conversation",
			Content: providerText(id), Provenance: []corememory.SourceRef{source},
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
		result, err := provider.Context(ctx, corememory.ContextRequest{
			Scope: scope, ConversationID: "conversation", Query: query, MinScore: .5,
			Budget: corememory.Budget{MaxItems: 5, MaxTokens: 100},
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
	result, err := provider.Context(context.Background(), corememory.ContextRequest{
		Scope: corememory.Scope{RuntimeID: "runtime"}, Query: "query",
		Budget: corememory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	diagnostics := provider.LastDiagnostics()
	if err != nil || len(result.Items) != 0 || len(diagnostics) != 1 || diagnostics[0].Lane != "vector" {
		t.Fatalf("all failed = %+v, diagnostics=%+v, err=%v", result, diagnostics, err)
	}
}

// TestSearchQueriesMergesSubQueries pins the multi-query path: each query
// contributes candidates, merged
// by identity, and a failing primary query still yields the sub-query hits.
// TestNormalizeRerankedItemsValidatesAndKeepsTheFullSet pins the item-ranker
// contract: the model may reorder and omit, but it cannot invent, duplicate,
// or lose items.
func TestNormalizeRerankedItemsValidatesAndKeepsTheFullSet(t *testing.T) {
	item := func(id string) corememory.ContextItem {
		return corememory.ContextItem{
			ID: id, Kind: corememory.ContextFact,
			Address: corememory.ContextAddress{Kind: corememory.ContextFact, ItemID: id},
		}
	}
	items := []corememory.ContextItem{item("a"), item("b"), item("c")}
	got, err := normalizeRerankedItems(items, []corememory.ContextItem{items[2], items[0]})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "c" || got[1].ID != "a" || got[2].ID != "b" {
		t.Fatalf("normalized = %v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
	unknown := item("zzz")
	if _, err := normalizeRerankedItems(items, []corememory.ContextItem{unknown}); err == nil {
		t.Fatal("unknown item accepted")
	}
	if _, err := normalizeRerankedItems(items, []corememory.ContextItem{items[0], items[0]}); err == nil {
		t.Fatal("duplicate item accepted")
	}
}

func TestSearchQueriesMergesSubQueries(t *testing.T) {
	scope := corememory.Scope{RuntimeID: "runtime"}
	primaryErr := errors.New("primary offline")
	search := providerSearcher(func(_ context.Context, request component.SearchRequest) ([]component.Candidate, error) {
		switch request.Query {
		case "primary":
			return []component.Candidate{providerCandidate("a", 1)}, nil
		case "failing":
			return nil, primaryErr
		case "sub":
			return []component.Candidate{providerCandidate("b", 1), providerCandidate("a", 0.4)}, nil
		default:
			return nil, nil
		}
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "lane", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &Provider{Fusion: fusor}
	diagnostics := []Diagnostic{}
	candidates, err := provider.searchQueries(
		context.Background(),
		corememory.ContextRequest{Scope: scope, Query: "primary"},
		nil, 10, []string{"primary", "sub"}, &diagnostics,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v", candidates)
	}
	// Cross-query RRF: "a" is ranked by both queries and wins over "b", which
	// only the sub-query ranks.
	if candidates[0].ID != "a" || candidates[0].Score <= candidates[1].Score {
		t.Fatalf("cross-query merge = %+v", candidates)
	}
	diagnostics = []Diagnostic{}
	candidates, err = provider.searchQueries(
		context.Background(),
		corememory.ContextRequest{Scope: scope, Query: "failing"},
		nil, 10, []string{"failing", "sub"}, &diagnostics,
	)
	// A failing lane is surfaced as a diagnostic by the fusion contract, so
	// the sub-query hits still come back and no error is raised.
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates = %+v, err = %v", candidates, err)
	}
	if len(diagnostics) == 0 {
		t.Fatal("failing primary lane was not reported as a diagnostic")
	}
}

func providerCandidate(id string, score float64) component.Candidate {
	return component.Candidate{
		ID: id, Lane: "native", Name: "fact", Score: score,
		Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"},
		Address: component.CandidateAddress{
			Kind: corememory.ContextFact, ConversationID: "conversation", ItemID: id,
		},
	}
}

// TestProviderParentExpansionGuardsCyclesAndDepth pins the two guards around
// chunk-hierarchy expansion: a repeated parent address cannot loop forever,
// and a long chain stops at maxParentDepth.
func TestProviderParentExpansionGuardsCyclesAndDepth(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: "leaf", Lane: "vector", Name: "chunk", Score: 1,
			Source: corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"},
			Address: component.CandidateAddress{
				Kind: corememory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "leaf",
			},
		}}, nil
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	cycle := &scriptedParentHydrator{mode: "cycle"}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: cycle, Packer: pack.New(nil), ExpandParents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "leaf", Budget: corememory.Budget{MaxItems: 64, MaxTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cycle.parentCalls > 1 || len(result.Items) != 1 {
		t.Fatalf("cycle calls=%d items=%d", cycle.parentCalls, len(result.Items))
	}
	reported := false
	for _, diagnostic := range provider.LastDiagnostics() {
		if diagnostic.Stage == "hydrate_parent" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("parent cycle was not reported as a diagnostic")
	}

	chain := &scriptedParentHydrator{mode: "chain"}
	provider, err = NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: chain, Packer: pack.New(nil), ExpandParents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "leaf", Budget: corememory.Budget{MaxItems: 64, MaxTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if chain.parentCalls != maxParentDepth || len(result.Items) != maxParentDepth+1 {
		t.Fatalf("chain calls=%d items=%d", chain.parentCalls, len(result.Items))
	}
}

type scriptedParentHydrator struct {
	mode        string
	parentCalls int
}

func (hydrator *scriptedParentHydrator) Hydrate(_ context.Context, _ corememory.Scope, candidate component.Candidate) (corememory.ContextItem, error) {
	return corememory.ContextItem{
		ID: candidate.ID, Kind: candidate.Address.Kind,
		Address: corememory.ContextAddress{
			Kind: candidate.Address.Kind, ConversationID: candidate.Address.ConversationID,
			DatasetID: candidate.Address.DatasetID, DocumentID: candidate.Address.DocumentID,
			ItemID: candidate.Address.ItemID,
		},
		Content: providerText("item " + candidate.ID), Score: candidate.Score,
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message"}},
		ParentID: "parent",
	}, nil
}

func (hydrator *scriptedParentHydrator) Parent(_ context.Context, _ corememory.Scope, item corememory.ContextItem) (corememory.ContextItem, bool, error) {
	hydrator.parentCalls++
	if hydrator.mode == "cycle" {
		// The parent resolves back to the same address as the current item.
		return item, true, nil
	}
	id := fmt.Sprintf("parent-%03d", hydrator.parentCalls)
	return corememory.ContextItem{
		ID: id, Kind: corememory.ContextDocumentSection,
		Address: corememory.ContextAddress{
			Kind: corememory.ContextDocumentSection, DatasetID: "dataset", DocumentID: "document", ItemID: id,
		},
		Content: providerText("parent"), Score: item.Score,
		Sources:  []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message"}},
		ParentID: "grandparent",
	}, true, nil
}

func providerText(value string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: value}}}
}
