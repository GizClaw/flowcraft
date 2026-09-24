package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	messagesource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

func TestProviderReturnsRecentForEmptyQueryAndAllHybridLanesFailed(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	messages := newMessageStore(t, newTestWorkspace(t))
	if _, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []coremessage.Message{
			coremessage.NewTextMessage(coremessage.RoleUser, "full user content"),
			coremessage.NewTextMessage(coremessage.RoleAssistant, "full assistant content"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	failed := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("lane unavailable")
	})
	fusor, err := fusion.New([]fusion.Lane{
		{Name: "vector", Searcher: failed, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "bm25", Searcher: failed, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "entity", Searcher: failed, Weight: 1, Calibrator: fusion.MinMax{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Messages: messages, Hydrator: &hydrate.Composite{Messages: messages},
		Packer: pack.New(nil), Recent: RecentConfig{MaxItems: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "",
		Budget: corememory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].MessageRole != coremessage.RoleUser ||
		result.Items[1].MessageRole != coremessage.RoleAssistant ||
		result.Items[0].Sequence != 1 || result.Items[1].Sequence != 2 ||
		result.Items[0].Content.Text() != "full user content" ||
		result.Items[0].Sources[0].Kind != corememory.SourceMessage {
		t.Fatalf("recent result = %#v", result)
	}
}

func newMessageStore(t *testing.T, ws workspace.Workspace) *messagesource.MessageStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := messagesource.NewMessageStore(logStore)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestProviderRecentPrioritySurvivesBudgetAndDeduplicatesHybrid(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	messages := newMessageStore(t, newTestWorkspace(t))
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []coremessage.Message{coremessage.NewTextMessage(coremessage.RoleUser, "recent")},
	})
	if err != nil {
		t.Fatal(err)
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: records[0].ID, Lane: "hybrid", Name: "message", Score: 1,
			Source:  corememory.SourceRef{Kind: corememory.SourceMessage, ID: "duplicate"},
			Address: component.CandidateAddress{Kind: corememory.ContextRawMessage, ConversationID: "conversation", ItemID: records[0].ID},
		}, providerCandidate("semantic", 1)}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{
		{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "bm25", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}},
		{Name: "entity", Searcher: search, Weight: 1, Calibrator: fusion.MinMax{}},
	})
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Messages: messages, Hydrator: &hydrate.Composite{Messages: messages},
		Packer: pack.New(nil), Recent: RecentConfig{MaxItems: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: corememory.Budget{MaxItems: 1, MaxTokens: 10},
	})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != records[0].ID ||
		result.Items[0].SourceClass != corememory.ContextSourceRecent {
		t.Fatalf("priority result = %#v, %v", result, err)
	}
}

// TestProviderBoundsOversizedNewestTurn pins the failure where the recent
// lane dropped the newest message entirely when it alone exceeded the lane
// budget. The turn must survive, bounded to the budget, and say so.
func TestProviderBoundsOversizedNewestTurn(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	messages := newMessageStore(t, newTestWorkspace(t))
	huge := strings.Repeat("x", 4000) // ~1000 estimated tokens
	if _, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []coremessage.Message{coremessage.NewTextMessage(coremessage.RoleUser, huge)},
	}); err != nil {
		t.Fatal(err)
	}
	failed := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("lane unavailable")
	})
	fusor, err := fusion.New([]fusion.Lane{
		{Name: "vector", Searcher: failed, Weight: 1, Calibrator: fusion.MinMax{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Messages: messages, Hydrator: &hydrate.Composite{Messages: messages},
		Packer: pack.New(nil), Recent: RecentConfig{MaxItems: 8, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation",
		Budget: corememory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want the newest turn to survive", len(result.Items))
	}
	item := result.Items[0]
	if item.SourceClass != corememory.ContextSourceRecent || item.TokenCount > 100 {
		t.Fatalf("item class/tokens = %q/%d, want recent/<=100", item.SourceClass, item.TokenCount)
	}
	if !result.Truncated {
		t.Fatal("result must report truncation after bounding the newest turn")
	}
	text := item.Content.Text()
	if text == "" || text == huge {
		t.Fatalf("content = %d runes, want a non-empty bounded prefix", len([]rune(text)))
	}
}

// TestProviderKeepsNewestTurnWhenOlderSiblingsFillTheBudget pins the second
// half of the same promise: an older turn in the same class must not push the
// newest one out. The recent lane budget here is larger than the request
// budget, so the lane hands all three turns to the packer and the packer
// decides -- admitting oldest-first would spend the class share and then the
// global budget on the older turns and drop the newest one.
func TestProviderKeepsNewestTurnWhenOlderSiblingsFillTheBudget(t *testing.T) {
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	messages := newMessageStore(t, newTestWorkspace(t))
	older := strings.Repeat("a", 160)  // ~40 tokens
	middle := strings.Repeat("b", 160) // ~40 tokens
	newest := strings.Repeat("z", 240) // ~60 tokens
	if _, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []coremessage.Message{
			coremessage.NewTextMessage(coremessage.RoleUser, older),
			coremessage.NewTextMessage(coremessage.RoleAssistant, middle),
			coremessage.NewTextMessage(coremessage.RoleUser, newest),
		},
	}); err != nil {
		t.Fatal(err)
	}
	failed := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return nil, errors.New("lane unavailable")
	})
	fusor, err := fusion.New([]fusion.Lane{
		{Name: "vector", Searcher: failed, Weight: 1, Calibrator: fusion.MinMax{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProviderWithConfig(ProviderConfig{
		Fusion: fusor, Messages: messages, Hydrator: &hydrate.Composite{Messages: messages},
		Packer: pack.New(nil), Recent: RecentConfig{MaxItems: 8, MaxTokens: 300},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Context(ctx, corememory.ContextRequest{
		Scope: scope, ConversationID: "conversation",
		Budget: corememory.Budget{MaxItems: 3, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	newestPresent := false
	for _, item := range result.Items {
		switch {
		case strings.Contains(item.Content.Text(), "zz"):
			newestPresent = true
		case item.SourceClass != corememory.ContextSourceRecent:
			t.Fatalf("item %q has source class %q, want recent", item.ID, item.SourceClass)
		}
	}
	if !newestPresent {
		ids := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			ids = append(ids, item.ID)
		}
		t.Fatalf("packed items %v dropped the newest turn", ids)
	}
	if !result.Truncated {
		t.Fatal("result must report truncation: the newer turn won the budget over older ones")
	}
}
