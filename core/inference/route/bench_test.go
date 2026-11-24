package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// BenchmarkGenerate is the per-call routing cost with a target that answers
// immediately: selection, the attempt bookkeeping and the response. A change that
// adds an allocation to the dispatch path shows up here.
func BenchmarkGenerate(b *testing.B) {
	generator := &fakeGenerator{reply: func(int) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{
			Message:      message.NewTextMessage(message.RoleAssistant, "ok"),
			FinishReason: inference.FinishStop,
		}, nil
	}}
	router, err := New([]Target{generateTarget("a", generator)}, Policy{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	request := generationRequest()
	ctx := context.Background()
	if _, err := router.Generate(ctx, request); err != nil {
		b.Fatalf("Generate: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := router.Generate(ctx, request); err != nil {
			b.Fatalf("Generate: %v", err)
		}
	}
}
