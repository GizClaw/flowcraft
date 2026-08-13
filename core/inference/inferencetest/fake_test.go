package inferencetest_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestGenerateFakeAssembly(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	assembly := fake.Assembly(t)

	request := inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
			}},
			Intent: inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
	response, err := assembly.Generate(
		context.Background(), inferencetest.DefaultFakeModel, request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(response.Message.Content.Parts))
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("compiler requests = %d, want 1", len(fake.Requests()))
	}
}
