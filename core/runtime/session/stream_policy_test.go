package session

import (
	"context"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestStreamPolicyContextRoundTrip(t *testing.T) {
	policy := StreamPolicy{
		Sinks:       []SinkSpec{{ID: "s1", Sink: discardSink{}}},
		Inheritable: true,
	}

	ctx := WithStreamPolicy(context.Background(), policy)
	got, ok := StreamPolicyFromContext(ctx)
	if !ok {
		t.Fatal("StreamPolicyFromContext: policy missing")
	}
	if !got.Inheritable || len(got.Sinks) != 1 || got.Sinks[0].ID != "s1" {
		t.Fatalf("StreamPolicyFromContext = %+v, want %+v", got, policy)
	}

	if _, ok := StreamPolicyFromContext(context.Background()); ok {
		t.Fatal("StreamPolicyFromContext reported a policy on a plain context")
	}

	// A nested stamp replaces the outer policy (nearest wins).
	inner := StreamPolicy{Sinks: nil, Inheritable: false}
	ctx = WithStreamPolicy(ctx, inner)
	got, ok = StreamPolicyFromContext(ctx)
	if !ok || got.Inheritable || len(got.Sinks) != 0 {
		t.Fatalf("nested policy = %+v, %v; want Inheritable=false with no sinks", got, ok)
	}
}

func TestStartStampsStreamPolicyIntoRunContext(t *testing.T) {
	var mu sync.Mutex
	var got StreamPolicy
	engine := agent.EngineFunc(func(
		ctx context.Context,
		_ agent.Run,
		_ agent.Host,
		board *agent.Board,
	) (*agent.Board, error) {
		policy, ok := StreamPolicyFromContext(ctx)
		if ok {
			mu.Lock()
			got = policy
			mu.Unlock()
		}
		board.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "done"))
		return board, nil
	})
	_, sess, _, _ := newTurnSession(t, engine, turnHostFactory)
	turn, err := sess.StartWithOptions(
		context.Background(),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		WithSinks(SinkSpec{ID: "s1", Sink: discardSink{}}),
	)
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	result, err := turn.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != agent.StatusCompleted {
		t.Fatalf("result status = %q, want completed", result.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if !got.Inheritable {
		t.Fatal("stamped policy Inheritable = false, want true")
	}
	if len(got.Sinks) != 1 || got.Sinks[0].ID != "s1" {
		t.Fatalf("stamped sinks = %+v, want [s1]", got.Sinks)
	}
}
