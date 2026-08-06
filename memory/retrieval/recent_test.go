package retrieval

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/memory/retrieval/pack"
	messagesource "github.com/GizClaw/flowcraft/memory/sources/message"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestProviderReturnsRecentForEmptyQueryAndAllHybridLanesFailed(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	messages, _ := messagesource.NewWorkspaceStore(workspace.NewMemWorkspace())
	if _, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{
			sdkmessage.NewTextMessage(sdkmessage.RoleUser, "full user content"),
			sdkmessage.NewTextMessage(sdkmessage.RoleAssistant, "full assistant content"),
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
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "",
		Budget: sdkmemory.Budget{MaxItems: 2, MaxTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].MessageRole != sdkmessage.RoleUser ||
		result.Items[1].MessageRole != sdkmessage.RoleAssistant ||
		result.Items[0].Sequence != 1 || result.Items[1].Sequence != 2 ||
		result.Items[0].Content.Text() != "full user content" ||
		result.Items[0].Sources[0].Kind != sdkmemory.SourceMessage {
		t.Fatalf("recent result = %#v", result)
	}
}

func TestProviderRecentPrioritySurvivesBudgetAndDeduplicatesHybrid(t *testing.T) {
	ctx := context.Background()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user"}
	messages, _ := messagesource.NewWorkspaceStore(workspace.NewMemWorkspace())
	records, err := messages.Append(ctx, messagesource.AppendRequest{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "recent")},
	})
	if err != nil {
		t.Fatal(err)
	}
	search := providerSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: records[0].ID, Lane: "hybrid", Name: "message", Score: 1,
			Source:  sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "duplicate"},
			Address: component.CandidateAddress{Kind: sdkmemory.ContextRawMessage, ConversationID: "conversation", ItemID: records[0].ID},
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
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "query",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 10},
	})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != records[0].ID ||
		result.Items[0].SourceClass != sdkmemory.ContextSourceRecent {
		t.Fatalf("priority result = %#v, %v", result, err)
	}
}
