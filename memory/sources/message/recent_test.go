package message

import (
	"context"
	"testing"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestLatestReturnsCanonicalWindowAscendingAndIsolated(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	store := newMessageStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time {
		now = now.Add(time.Second)
		return now
	}))
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent-a"}
	for index, text := range []string{"one", "two", "three", "four"} {
		if _, err := store.Append(ctx, AppendRequest{
			Scope: scope, ConversationID: "conversation", IdempotencyKey: text,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, text)},
		}); err != nil {
			t.Fatal(err)
		}
		_ = index
	}
	got, err := store.Latest(ctx, scope, "conversation", LatestOptions{Limit: 2})
	if err != nil || len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 ||
		got[0].Message.Content.Text() != "three" || !got[0].CreatedAt.Before(got[1].CreatedAt) {
		t.Fatalf("latest = %#v, %v", got, err)
	}
	otherAgent := scope
	otherAgent.AgentID = "agent-b"
	isolated, err := store.Latest(ctx, otherAgent, "conversation", LatestOptions{Limit: 2})
	if err != nil || len(isolated) != 0 {
		t.Fatalf("cross-agent leak = %#v, %v", isolated, err)
	}
	otherConversation, err := store.Latest(ctx, scope, "other", LatestOptions{Limit: 2})
	if err != nil || len(otherConversation) != 0 {
		t.Fatalf("cross-conversation leak = %#v, %v", otherConversation, err)
	}
}
