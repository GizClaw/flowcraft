package pipeline_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// This file is the baseline for the per-request pipeline: compile, transport and
// decode run once per call, so an allocation added anywhere in the three stages
// shows up here instead of in a p99 nobody attributes to inference.

type wireRequest struct {
	Model string
	Text  string
}

type wireResponse struct {
	Text string
}

func BenchmarkBindGenerate(b *testing.B) {
	driver, err := pipeline.BindGenerate("bench",
		func(_ context.Context, request inference.GenerateRequest, _ pipeline.Shape) (pipeline.Compiled[wireRequest], error) {
			return pipeline.Compiled[wireRequest]{
				Wire: wireRequest{Model: request.Model.ID.Name, Text: request.Input.Content.Text()},
				// The contract check requires every active field to be accounted
				// for: this compiler honours everything the caller set.
				Fields: request.ActiveFields(),
			}, nil
		},
		func(_ context.Context, _ inference.GenerateRequest, wire wireRequest) (wireResponse, error) {
			return wireResponse{Text: wire.Text}, nil
		},
		func(_ context.Context, _ inference.GenerateRequest, raw wireResponse) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message:      message.NewTextMessage(message.RoleAssistant, raw.Text),
				FinishReason: inference.FinishStop,
			}, nil
		},
	)
	if err != nil {
		b.Fatalf("BindGenerate: %v", err)
	}

	request := inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "bench", Name: "m"}},
		Input: inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("hello there")},
	}
	ctx := context.Background()

	// Warm up once: the first call also derives the extension set and would
	// otherwise be charged to the first iteration.
	if _, err := driver.Execute(ctx, request); err != nil {
		b.Fatalf("Execute: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := driver.Execute(ctx, request); err != nil {
			b.Fatalf("Execute: %v", err)
		}
	}
}
