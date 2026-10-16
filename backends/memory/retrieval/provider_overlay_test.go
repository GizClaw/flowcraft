package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// adjusterBase makes each double a full ScoreAdjuster: the provider holds the
// single-identity form, and the bulk and scoped reads are assertions on it.
type adjusterBase struct{}

func (adjusterBase) Adjust(context.Context, corememory.Scope, string) (float64, error) {
	return 1, nil
}

// scopedAdjuster is the per-conversation overlay read: it records what a
// request asked to resolve and answers with the factors the fixture planted.
type scopedAdjuster struct {
	adjusterBase
	factors       map[string]float64
	err           error
	conversations []string
	calls         int
}

func (adjuster *scopedAdjuster) ScoreOverlayFor(
	_ context.Context,
	_ corememory.Scope,
	conversations []string,
) (map[string]float64, error) {
	adjuster.calls++
	adjuster.conversations = append(adjuster.conversations, conversations...)
	return adjuster.factors, adjuster.err
}

// bulkAdjuster is the whole-scope read and only that: the shape an
// implementation without a per-conversation read still offers.
type bulkAdjuster struct {
	adjusterBase
	factors map[string]float64
	calls   int
}

func (adjuster *bulkAdjuster) ScoreOverlay(context.Context, corememory.Scope) (map[string]float64, error) {
	adjuster.calls++
	return adjuster.factors, nil
}

// bothAdjuster offers both reads, as the maintenance store does: the scoped
// one is the one a request should pay for.
type bothAdjuster struct {
	adjusterBase
	factors     map[string]float64
	scopedCalls int
	bulkCalls   int
}

func (adjuster *bothAdjuster) ScoreOverlayFor(
	context.Context,
	corememory.Scope,
	[]string,
) (map[string]float64, error) {
	adjuster.scopedCalls++
	return adjuster.factors, nil
}

func (adjuster *bothAdjuster) ScoreOverlay(context.Context, corememory.Scope) (map[string]float64, error) {
	adjuster.bulkCalls++
	return adjuster.factors, nil
}

// overlayFixture seeds three conversations and a lane whose candidates name two
// of them, so a request's candidates span more than one conversation and less
// than the scope.
func overlayFixture(t *testing.T) (
	*factview.FactStore,
	corememory.Scope,
	*fusion.Fusion,
	func(conversation string, score float64) component.Candidate,
) {
	t.Helper()
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := newFactStore(t, newTestWorkspace(t))
	source := corememory.SourceRef{Kind: corememory.SourceMessage, ID: "message"}
	for _, conversation := range []string{"conversation-a", "conversation-b", "conversation-c"} {
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: "fact-1", Generation: testGeneration, Scope: scope, ConversationID: conversation,
			Content: providerText(conversation), Provenance: []corememory.SourceRef{source},
		}); err != nil {
			t.Fatal(err)
		}
	}
	candidate := func(conversation string, score float64) component.Candidate {
		return component.Candidate{
			ID: "projection-" + conversation, Lane: "vector", Name: "fact", Score: score, Source: source,
			Address: component.CandidateAddress{
				Kind: corememory.ContextFact, ConversationID: conversation, ItemID: "fact-1",
			},
		}
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{candidate("conversation-a", 1), candidate("conversation-c", 1)}, nil
	})
	fusor, err := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	if err != nil {
		t.Fatal(err)
	}
	return facts, scope, fusor, candidate
}

// TestProviderReadsOnlyTheCandidateConversationsOverlay pins the scoped read
// at the request: the overlay is read for the conversations the candidates
// name -- not for the scope, and not for a conversation the request cannot
// draw from -- and the factor it returns is applied to the item.
func TestProviderReadsOnlyTheCandidateConversationsOverlay(t *testing.T) {
	ctx := context.Background()
	facts, scope, fusor, _ := overlayFixture(t)
	identity := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conversation-a", ItemID: "fact-1",
	}.Identity(scope)
	scoped := &scopedAdjuster{factors: map[string]float64{identity: 0.5}}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		ScoreAdjuster: scoped,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "query", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.calls != 1 {
		t.Fatalf("scoped overlay reads = %d, want one per request", scoped.calls)
	}
	if len(scoped.conversations) != 2 || scoped.conversations[0] != "conversation-a" || scoped.conversations[1] != "conversation-c" {
		t.Fatalf("scoped read named %#v, want the candidates' conversations", scoped.conversations)
	}
	adjusted := false
	for _, item := range result.Items {
		if item.Identity(scope) != identity {
			continue
		}
		adjusted = true
		if item.Score != 0.5 {
			t.Fatalf("adjusted score = %v, want the candidate's score halved", item.Score)
		}
	}
	if !adjusted {
		t.Fatalf("the adjusted conversation is missing from %+v", result.Items)
	}
}

// TestProviderFallsBackToTheWholeScopeOverlay pins the compatibility half: an
// adjuster that only resolves a whole scope is still read, and a failing read
// is a diagnostic rather than a failed request.
func TestProviderFallsBackToTheWholeScopeOverlay(t *testing.T) {
	ctx := context.Background()
	facts, scope, fusor, _ := overlayFixture(t)
	identity := corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conversation-a", ItemID: "fact-1",
	}.Identity(scope)
	bulk := &bulkAdjuster{factors: map[string]float64{identity: 0.25}}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		ScoreAdjuster: bulk,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "query", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bulk.calls != 1 {
		t.Fatalf("bulk overlay reads = %d, want one", bulk.calls)
	}
	for _, item := range result.Items {
		if item.Identity(scope) == identity && item.Score != 0.25 {
			t.Fatalf("adjusted score = %v, want the candidate's score quartered", item.Score)
		}
	}

	failing := &scopedAdjuster{err: errors.New("overlay offline")}
	provider, err = NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		ScoreAdjuster: failing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "query", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	}); err != nil {
		t.Fatalf("a failing overlay read failed the request: %v", err)
	}
	reported := false
	for _, diagnostic := range provider.LastDiagnostics() {
		reported = reported || diagnostic.Stage == "score_adjust"
	}
	if !reported {
		t.Fatalf("failing overlay read was not reported: %+v", provider.LastDiagnostics())
	}
}

// TestProviderPrefersTheScopedOverlayWhenBothAreOffered pins the choice an
// implementation offering both reads gets: the scoped one.
func TestProviderPrefersTheScopedOverlayWhenBothAreOffered(t *testing.T) {
	ctx := context.Background()
	facts, scope, fusor, _ := overlayFixture(t)
	both := &bothAdjuster{}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		ScoreAdjuster: both,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, Query: "query", Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if both.scopedCalls != 1 || both.bulkCalls != 0 {
		t.Fatalf("scoped=%d bulk=%d, want the scoped read alone", both.scopedCalls, both.bulkCalls)
	}
}
