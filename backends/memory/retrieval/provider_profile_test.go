package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/profile"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// fakeProfile is a block store in a variable: it records what the request asked
// it for and answers with the block one test wants served.
type fakeProfile struct {
	block         profile.Block
	err           error
	conversations []string
	hide          func(factID string) (bool, error)
}

func (source *fakeProfile) BlockServing(
	_ context.Context,
	_ corememory.Scope,
	conversationID string,
	hide func(factID string) (bool, error),
) (profile.Block, int, error) {
	source.conversations = append(source.conversations, conversationID)
	source.hide = hide
	if source.err != nil {
		return profile.Block{}, 0, source.err
	}
	return source.block, 0, nil
}

// failingVisibility is a ledger whose whole-scope read is unreadable and whose
// per-identity read fails for one identity: the shape one corrupt record leaves
// behind, which is where the two failure directions meet.
type failingVisibility struct {
	fail  string
	calls int
}

func (visibility *failingVisibility) SoftForgotten(context.Context, corememory.Scope) (map[string]struct{}, error) {
	return nil, errors.New("ledger corrupt")
}

func (visibility *failingVisibility) Visible(_ context.Context, _ corememory.Scope, identity string) (bool, error) {
	visibility.calls++
	if identity == visibility.fail {
		return false, errors.New("record corrupt")
	}
	return true, nil
}

func profileBlock() profile.Block {
	return profile.Block{
		Text:    "- Melanie loves pottery.\n- Melanie plays the cello.",
		Entries: 2,
		Sources: []corememory.SourceRef{
			{Kind: corememory.SourceExternal, ID: "profile/conversation/one", Locator: "facts:fact-1"},
			{Kind: corememory.SourceExternal, ID: "profile/conversation/two", Locator: "facts:fact-2"},
		},
	}
}

func profileProvider(t *testing.T, facts *factview.FactStore, visibility Visibility, source ProfileSource) *Provider {
	t.Helper()
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{providerCandidate("served", .5), providerCandidate("hidden", .4)}, nil
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		Visibility: visibility, Profiles: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func profileRequest(scope corememory.Scope, conversationID string) corememory.ContextRequest {
	return corememory.ContextRequest{
		Scope: scope, ConversationID: conversationID, Query: "what does she do on weekends",
		Budget: corememory.Budget{MaxItems: 8, MaxTokens: 400},
	}
}

// TestProviderServesTheResidentBlockOnEveryRequest pins what the block is: an
// addition to a request rather than a candidate. The query does not match it,
// the score floor does not judge it, and it is served first -- that is the
// whole point of the resident half, and the reason a deployment wires it only
// when every request carrying it is what it wants.
func TestProviderServesTheResidentBlockOnEveryRequest(t *testing.T) {
	scope, facts, _ := twoFacts(t)
	source := &fakeProfile{block: profileBlock()}
	provider := profileProvider(t, facts, nil, source)

	request := profileRequest(scope, "conversation")
	request.MinScore = 1 // Above both lanes' scores: nothing retrieved is served.
	result, err := provider.Context(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.conversations) != 1 || source.conversations[0] != "conversation" {
		t.Fatalf("block reads = %#v", source.conversations)
	}
	if len(result.Items) != 1 || result.Items[0].Kind != corememory.ContextProfile {
		t.Fatalf("items = %+v", result.Items)
	}
	item := result.Items[0]
	if item.SourceClass != corememory.ContextSourceProfile || item.Content.Text() != source.block.Text {
		t.Fatalf("item = %+v", item)
	}
	if item.ID != "conversation" || item.Address.Kind != corememory.ContextProfile ||
		item.Address.ConversationID != "conversation" {
		t.Fatalf("item address = %+v id = %q", item.Address, item.ID)
	}
	if len(item.Sources) != len(source.block.Sources) {
		t.Fatalf("item sources = %+v", item.Sources)
	}
	if item.Score != 1 || item.TokenCount == 0 {
		t.Fatalf("item score = %v tokens = %d", item.Score, item.TokenCount)
	}
}

// TestProviderServesNoBlockWithoutAConversation pins the boundary: the block is
// one conversation's lines about its participants, so a request that names no
// conversation has none to serve and the store is never read.
func TestProviderServesNoBlockWithoutAConversation(t *testing.T) {
	scope, facts, _ := twoFacts(t)
	source := &fakeProfile{block: profileBlock()}
	provider := profileProvider(t, facts, nil, source)

	result, err := provider.Context(context.Background(), profileRequest(scope, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.conversations) != 0 {
		t.Fatalf("block reads = %#v, want none", source.conversations)
	}
	for _, item := range result.Items {
		if item.Kind == corememory.ContextProfile {
			t.Fatalf("a request without a conversation was served a block: %+v", result.Items)
		}
	}
}

// TestProviderReportsABlockItCannotRead pins the failure direction: the block
// is derived, so it is not what fails a request the canonical views can answer.
// The request serves its facts, reports the read, and carries no block.
func TestProviderReportsABlockItCannotRead(t *testing.T) {
	scope, facts, _ := twoFacts(t)
	source := &fakeProfile{err: errors.New("blocks unreadable")}
	provider := profileProvider(t, facts, nil, source)

	result, err := provider.Context(context.Background(), profileRequest(scope, "conversation"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items = %+v, want the two facts", result.Items)
	}
	if !hasDiagnostic(provider.LastDiagnostics(), "profile") {
		t.Fatalf("diagnostics = %#v", provider.LastDiagnostics())
	}
}

// TestProviderAsksTheOverlayAboutTheBlocksProvenance pins the wire between the
// two: the block is served through the same visibility the candidates are, so
// the store is handed a question about fact ids that resolves the identity the
// read path hides -- and one the read path cannot decide about is reported as
// hidden rather than as nothing.
func TestProviderAsksTheOverlayAboutTheBlocksProvenance(t *testing.T) {
	scope, facts, hidden := twoFacts(t)
	visibility := &bulkHidden{hiddenSet: hiddenSet{hidden: map[string]struct{}{hidden: {}}}}
	source := &fakeProfile{block: profileBlock()}
	provider := profileProvider(t, facts, visibility, source)

	if _, err := provider.Context(context.Background(), profileRequest(scope, "conversation")); err != nil {
		t.Fatal(err)
	}
	if source.hide == nil {
		t.Fatal("the block store was asked to serve without the overlay's filter")
	}
	forgotten, err := source.hide("hidden")
	if err != nil || !forgotten {
		t.Fatalf("hide(hidden) = %t, %v", forgotten, err)
	}
	served, err := source.hide("served")
	if err != nil || served {
		t.Fatalf("hide(served) = %t, %v", served, err)
	}

	// One unreadable record: the request still answers from what it could
	// decide, and the block's filter reports that it cannot decide.
	failing := &failingVisibility{fail: factIdentity(scope, "conversation", "hidden")}
	provider = profileProvider(t, facts, failing, source)
	if _, err := provider.Context(context.Background(), profileRequest(scope, "conversation")); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := source.hide("hidden"); !forgotten || err == nil {
		t.Fatalf("hide(hidden) with an unreadable record = %t, %v", forgotten, err)
	}
	if forgotten, err := source.hide("served"); forgotten || err != nil {
		t.Fatalf("hide(served) with an unreadable record = %t, %v", forgotten, err)
	}
}
