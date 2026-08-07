package config

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestAssemblyAppliesRetryPolicyFromJSON(t *testing.T) {
	calls := 0
	driver, err := inference.BindGenerate(
		func(
			_ context.Context,
			_ inference.ModelRef,
			request inference.GenerateRequest,
			shape inference.GenerateExecutionShape,
		) (inference.Compiled[string], error) {
			active := request.ActiveFieldsFor(shape)
			decisions := make([]inference.Decision, len(active))
			for index, field := range active {
				decisions[index] = inference.Decision{
					Field: field, Disposition: inference.Native,
				}
			}
			return inference.Compiled[string]{
				Wire: "wire",
				Report: inference.CompileReport{
					Operation: inference.OperationGenerate, Decisions: decisions,
				},
			}, nil
		},
		func(context.Context, string) (string, error) {
			calls++
			if calls == 1 {
				return "", errdefs.RateLimit(errors.New("slow down"))
			}
			return "ok", nil
		},
		func(context.Context, string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "ok"}},
					},
				},
				FinishReason: inference.FinishCompleted,
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	builder, err := NewBuilder(
		map[string]Factory{"fake": FactoryFunc(func(
			_ context.Context,
			input ProviderInput,
		) (inference.ProviderDefinition, error) {
			return inference.ProviderDefinition{
				ID: input.ID,
				Models: []inference.ModelImplementation{{
					Descriptor: inference.ModelDescriptor{
						ID: inference.ModelID{Provider: input.ID, Name: "model"},
					},
					Openers: inference.Openers{
						Generate: func(
							context.Context,
							inference.ModelRef,
						) (inference.GenerateOperations, error) {
							return inference.GenerateOperations{Unary: driver}, nil
						},
					},
				}},
			}, nil
		})},
		nil,
	)
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}

	document, err := Parse([]byte(`{
		"version": "v1",
		"providers": [{"id": "fake", "driver": "fake"}],
		"route": {
			"generate": [{
				"tier": "primary",
				"targets": [{"model": {
					"id": {"provider": "fake", "name": "model"}
				}}]
			}],
			"retry": {
				"generate": {
					"max_attempts": 2,
					"backoff": {"kind": "fixed", "initial": "1ms"},
					"retryable": ["rate_limit"]
				}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	assembly, err := builder.NewAssembly(context.Background(), document)
	if err != nil {
		t.Fatalf("NewAssembly: %v", err)
	}
	if assembly.Router == nil {
		t.Fatal("route section did not compose a Router")
	}

	response, trace, err := assembly.Router.Generate(
		context.Background(),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "hi"}},
					},
					Intent: inference.Intent{Text: &inference.TextIntent{}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("transport calls = %d, want 2", calls)
	}
	if response.Metadata.Model.Name != "model" {
		t.Fatalf("executed = %+v", response.Metadata.Model)
	}
	retries := 0
	for _, attempt := range trace.Attempts {
		if attempt.Trigger == route.AttemptTriggerRetry {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry triggers = %d, trace = %+v", retries, trace.Attempts)
	}
}
