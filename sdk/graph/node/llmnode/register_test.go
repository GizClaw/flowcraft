package llmnode_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/node/llmnode"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

type stubResolver struct{}

func (s *stubResolver) Resolve(_ context.Context, _ string) (llm.LLM, error) { return nil, nil }
func (s *stubResolver) InvalidateCache(_ string)                             {}

func TestRegister_NilResolverFailsBuild(t *testing.T) {
	f := node.NewFactory()
	llmnode.Register(f, nil, nil)

	_, err := f.Build(graph.NodeDefinition{ID: "llm1", Type: "llm"})
	if err == nil {
		t.Fatal("expected error when LLMResolver is nil")
	}
}

func TestRegister_HappyPath(t *testing.T) {
	f := node.NewFactory()
	llmnode.Register(f, &stubResolver{}, nil)

	n, err := f.Build(graph.NodeDefinition{
		ID:   "llm1",
		Type: "llm",
		Config: map[string]any{
			"system_prompt": "be helpful",
			"temperature":   0.5,
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if n.ID() != "llm1" || n.Type() != "llm" {
		t.Fatalf("identity mismatch: id=%q type=%q", n.ID(), n.Type())
	}
}
