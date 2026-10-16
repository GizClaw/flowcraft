package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	documentview "github.com/GizClaw/flowcraft/backends/memory/views/document"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// hiddenSet answers the per-identity overlay read from a set, and counts the
// reads: a test that pins which read a request pays for needs both.
type hiddenSet struct {
	hidden map[string]struct{}
	err    error
	calls  int
}

func (set *hiddenSet) Visible(_ context.Context, _ corememory.Scope, identity string) (bool, error) {
	set.calls++
	if set.err != nil {
		return false, set.err
	}
	_, forgotten := set.hidden[identity]
	return !forgotten, nil
}

// bulkHidden answers the whole-scope read instead -- the read a request pays
// for when the substrate offers it -- and counts the reads separately, so a
// test can tell the bulk read from the per-identity fallback.
type bulkHidden struct {
	hiddenSet
	bulkCalls int
	bulkErr   error
}

func (set *bulkHidden) SoftForgotten(context.Context, corememory.Scope) (map[string]struct{}, error) {
	set.bulkCalls++
	if set.bulkErr != nil {
		return nil, set.bulkErr
	}
	return set.hidden, nil
}

func factIdentity(scope corememory.Scope, conversationID, factID string) string {
	return corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: conversationID, ItemID: factID,
	}.Identity(scope)
}

// twoFacts stores one forgotten fact beside one that is served, and returns the
// scope and the identity the overlay hides.
func twoFacts(t *testing.T) (scope corememory.Scope, facts *factview.FactStore, hidden string) {
	t.Helper()
	scope = corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	facts = newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, id := range []string{"hidden", "served"} {
		if _, err := facts.Add(context.Background(), factview.AddRequest{
			ID: id, Generation: testGeneration, Scope: scope, ConversationID: "conversation",
			Content: providerText(id), Provenance: []corememory.SourceRef{source},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return scope, facts, factIdentity(scope, "conversation", "hidden")
}

func twoFactProvider(t *testing.T, facts *factview.FactStore, visibility Visibility) *Provider {
	t.Helper()
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("hidden", 1), providerCandidate("served", .5)}, nil
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil), Visibility: visibility,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func servedIDs(provider *Provider, scope corememory.Scope) ([]string, error) {
	result, err := provider.Context(context.Background(), corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "hidden served",
		Budget: corememory.Budget{MaxItems: 4, MaxTokens: 200},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		ids = append(ids, item.ID)
	}
	return ids, nil
}

// TestProviderServesTheBulkOverlayInOneRead pins the shape the assembly wires:
// the ledger answers the whole scope in one read, the request pays for that
// read and not for one lookup per candidate, and the forgotten item is not in
// the result while the rest of it is untouched.
func TestProviderServesTheBulkOverlayInOneRead(t *testing.T) {
	scope, facts, hidden := twoFacts(t)
	visibility := &bulkHidden{hiddenSet: hiddenSet{hidden: map[string]struct{}{hidden: {}}}}
	provider := twoFactProvider(t, facts, visibility)

	ids, err := servedIDs(provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "served" {
		t.Fatalf("served ids = %#v", ids)
	}
	if visibility.bulkCalls != 1 || visibility.calls != 0 {
		t.Fatalf("bulk reads = %d, per-item reads = %d", visibility.bulkCalls, visibility.calls)
	}
}

// TestProviderFallsBackToPerItemVisibility pins the substrate without a bulk
// read, and one whose bulk read failed: the request still asks per item, the
// forgotten item is still not served, and the failed bulk read is in the
// diagnostics rather than being read as "nothing is forgotten".
func TestProviderFallsBackToPerItemVisibility(t *testing.T) {
	scope, facts, hidden := twoFacts(t)
	failing := &bulkHidden{
		hiddenSet: hiddenSet{hidden: map[string]struct{}{hidden: {}}},
		bulkErr:   errors.New("ledger unavailable"),
	}
	provider := twoFactProvider(t, facts, failing)
	ids, err := servedIDs(provider, scope)
	if err != nil || len(ids) != 1 || ids[0] != "served" {
		t.Fatalf("served ids = %#v, %v", ids, err)
	}
	if failing.bulkCalls != 1 || failing.calls != 2 {
		t.Fatalf("bulk reads = %d, per-item reads = %d", failing.bulkCalls, failing.calls)
	}
	if !hasDiagnostic(provider.LastDiagnostics(), "visibility") {
		t.Fatalf("diagnostics = %#v", provider.LastDiagnostics())
	}

	perItem := &hiddenSet{hidden: map[string]struct{}{hidden: {}}}
	provider = twoFactProvider(t, facts, perItem)
	if ids, err := servedIDs(provider, scope); err != nil || len(ids) != 1 || ids[0] != "served" {
		t.Fatalf("served ids = %#v, %v", ids, err)
	}
	if perItem.calls != 2 {
		t.Fatalf("per-item reads = %d", perItem.calls)
	}
}

// TestProviderFailsRatherThanAnsweringPastAnUnreadableOverlay pins what an
// overlay nobody can read does. An item whose visibility cannot be decided is
// not an eligible item: the request drops it and reports the read, and when
// that leaves nothing to serve the request fails -- the same choice a lane that
// cannot read the state it is bound to read makes -- instead of answering like
// an ordinary memory with the overlay's items gone quiet.
func TestProviderFailsRatherThanAnsweringPastAnUnreadableOverlay(t *testing.T) {
	scope, facts, _ := twoFacts(t)
	provider := twoFactProvider(t, facts, &hiddenSet{err: errors.New("ledger corrupt")})
	if _, err := servedIDs(provider, scope); err == nil {
		t.Fatal("a request past an unreadable overlay was served")
	}
	if !hasDiagnostic(provider.LastDiagnostics(), "visibility") {
		t.Fatalf("diagnostics = %#v", provider.LastDiagnostics())
	}
}

// documentHierarchy stores one document's chunk, section, and resource, and
// returns the scope, the views, and the identity behind the candidate the
// searcher returns.
func documentHierarchy(t *testing.T) (corememory.Scope, *documentview.DocumentViewStore) {
	t.Helper()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	kvStore, err := storage.NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	documents, err := documentview.NewDocumentViewStore(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	source := corememory.SourceRef{Kind: corememory.SourceDocument, ID: "dataset/document", Revision: "1"}
	metadata := corememory.Metadata{"dataset_id": "dataset", "document_id": "document"}
	records := []documentview.Chunk{
		{
			ID: "resource", Kind: documentview.KindResource, Level: 0, Scope: scope, DatasetID: "dataset",
			DocumentID: "document", DocumentVersion: 1, Content: providerText("resource"),
			Provenance: []corememory.SourceRef{source}, Metadata: metadata,
		},
		{
			ID: "section", Kind: documentview.KindSection, Level: 1, ParentID: "resource", Scope: scope,
			DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("section"),
			Provenance: []corememory.SourceRef{source}, Metadata: metadata,
		},
		{
			ID: "chunk", Kind: documentview.KindChunk, Level: 2, ParentID: "section", Scope: scope,
			DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Content: providerText("chunk"),
			Provenance: []corememory.SourceRef{source}, Metadata: metadata,
		},
	}
	if _, err := documents.ReplaceDocument(context.Background(), documentview.ReplaceRequest{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", DocumentVersion: 1, Chunks: records,
	}); err != nil {
		t.Fatal(err)
	}
	return scope, documents
}

func documentProvider(t *testing.T, documents *documentview.DocumentViewStore, hidden map[string]struct{}) *Provider {
	t.Helper()
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: "chunk", Lane: "vector", Name: "chunk", Score: 1, Source: corememory.SourceRef{Kind: corememory.SourceDocument, ID: "dataset/document", Revision: "1"},
			Address: component.CandidateAddress{
				Kind: corememory.ContextDocumentChunk, DatasetID: "dataset", DocumentID: "document", ItemID: "chunk",
			},
		}}, nil
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}}})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Chunks: documents}, Packer: pack.New(nil),
		ExpandParents: true, Visibility: &hiddenSet{hidden: hidden},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// TestProviderStopsTheParentChainAtAForgottenRecord pins the recursive half of
// a forget: a record the overlay hides does not come back as somebody's parent,
// and neither does anything above it -- a chain that walked past a forgotten
// section would serve the section's own text one level up -- while a forget
// below the top of the chain keeps serving what is above it.
func TestProviderStopsTheParentChainAtAForgottenRecord(t *testing.T) {
	scope, documents := documentHierarchy(t)
	identity := func(kind corememory.ContextItemKind, itemID string) string {
		return corememory.ContextAddress{
			Kind: kind, DatasetID: "dataset", DocumentID: "document", ItemID: itemID,
		}.Identity(scope)
	}
	cases := []struct {
		name   string
		hidden map[string]struct{}
		served []string
	}{
		{name: "nothing forgotten", served: []string{"chunk", "section", "resource"}},
		{
			name:   "the candidate itself",
			hidden: map[string]struct{}{identity(corememory.ContextDocumentChunk, "chunk"): {}},
		},
		{
			name:   "the middle of the chain",
			hidden: map[string]struct{}{identity(corememory.ContextDocumentSection, "section"): {}},
			served: []string{"chunk"},
		},
		{
			name:   "the top of the chain",
			hidden: map[string]struct{}{identity(corememory.ContextDocumentResource, "resource"): {}},
			served: []string{"chunk", "section"},
		},
	}
	for _, testCase := range cases {
		provider := documentProvider(t, documents, testCase.hidden)
		result, err := provider.Context(context.Background(), corememory.ContextRequest{
			Scope: scope, Query: "chunk", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
		})
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		ids := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			ids = append(ids, item.ID)
		}
		if len(ids) != len(testCase.served) {
			t.Fatalf("%s: served %#v, want %#v", testCase.name, ids, testCase.served)
		}
		for index, id := range testCase.served {
			if ids[index] != id {
				t.Fatalf("%s: served %#v, want %#v", testCase.name, ids, testCase.served)
			}
		}
	}
}

func hasDiagnostic(diagnostics []Diagnostic, stage string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Stage == stage {
			return true
		}
	}
	return false
}
