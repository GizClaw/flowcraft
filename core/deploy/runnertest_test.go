package deploy

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	a2a "github.com/GizClaw/flowcraft/core/agent-a2a"
	"github.com/GizClaw/flowcraft/core/agent-a2a/runnertest"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// alwaysText answers every round. The conformance suite builds a fresh runner per
// subtest, so a step list would run out.
type alwaysText struct{ text string }

func (p alwaysText) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return &stream{
		events: []inference.StreamEvent{{Kind: inference.StreamText, Text: p.text}},
		resp:   inference.GenerateResponse{Message: message.NewTextMessage(message.RoleAssistant, p.text)},
	}, nil
}

func (p alwaysText) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	inner, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	return inner.Result()
}

// A local session and a proxy must look identical to the protocol adapter.
func TestLocalSessionRunnerConformance(t *testing.T) {
	rt, err := runtime.New(context.Background(), config(t), driver(alwaysText{text: "hello"}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	runnertest.Runner(t, func(t *testing.T) a2a.Runner {
		session, err := agent.NewSession(rt)
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return session
	})
}

func TestProxyRunnerConformance(t *testing.T) {
	srv := backend(t, alwaysText{text: "hello from backend"})
	runnertest.Runner(t, func(t *testing.T) a2a.Runner {
		conversation, err := newProxyConversation(context.Background(),
			EngineSpec{Kind: "a2a", Peer: srv.URL}, nil, "", "conv-1")
		if err != nil {
			t.Fatalf("newProxyConversation: %v", err)
		}
		return conversation
	})
}
