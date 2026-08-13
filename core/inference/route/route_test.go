package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
)

type routeWire struct{}
type routeRaw struct{ Text string }

func routeCompiler() inference.GenerateCompiler[routeWire] {
	return func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[routeWire], error) {
		active := request.ActiveFieldsFor(shape)
		decisions := make([]inference.Decision, len(active))
		for index, field := range active {
			decisions[index] = inference.Decision{Field: field, Disposition: inference.Native}
		}
		return inference.Compiled[routeWire]{
			Wire: routeWire{},
			Report: inference.CompileReport{
				Operation: inference.OperationGenerate,
				Decisions: decisions,
			},
		}, nil
	}
}

func routeTransport(fail bool) inference.Transport[routeWire, routeRaw] {
	return func(context.Context, routeWire) (routeRaw, error) {
		if fail {
			return routeRaw{}, errdefs.NotAvailablef("upstream unavailable")
		}
		return routeRaw{Text: "ok"}, nil
	}
}

func routeDecode() inference.Decoder[routeRaw, inference.GenerateResponse] {
	return func(_ context.Context, raw routeRaw) (inference.GenerateResponse, error) {
		return inference.GenerateResponse{
			Message: message.Message{
				Role: message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: raw.Text},
				}},
			},
			FinishReason: inference.FinishCompleted,
			Usage: inference.Usage{
				InputTokens:  3,
				OutputTokens: 4,
				TotalTokens:  7,
			},
		}, nil
	}
}

func providerDefinition(
	t *testing.T,
	id string,
	fail bool,
) inference.ProviderDefinition {
	t.Helper()
	driver, err := inference.BindGenerate(
		routeCompiler(),
		routeTransport(fail),
		routeDecode(),
	)
	if err != nil {
		t.Fatalf("BindGenerate: %v", err)
	}
	return inference.ProviderDefinition{
		ID: id,
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: id, Name: "model-1"},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context, inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		}},
	}
}

func newRouteAssembly(t *testing.T) *inference.Assembly {
	t.Helper()
	factory := inference.Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{
			"provider.bad":  providerDefinition(t, "bad", true),
			"provider.good": providerDefinition(t, "good", false),
		},
	})
	if err != nil {
		t.Fatalf("build assembly: %v", err)
	}
	return value.(*inference.Assembly)
}

func routeRequest() inference.GenerateRequest {
	return inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "hello"},
			}},
			Intent: inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
}

func TestRouterGenerateFallsBackAcrossPools(t *testing.T) {
	assembly := newRouteAssembly(t)
	policy := Policy{
		Generate: []Pool{
			{Tier: "primary", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "bad", Name: "model-1"},
				},
			}}},
			{Tier: "fallback", Targets: []Target{{
				Model: inference.ModelRef{
					ID: inference.ModelID{Provider: "good", Name: "model-1"},
				},
			}}},
		},
		Retry: &RetryConfig{
			Generate: &RetryPolicyConfig{
				MaxAttempts:              1,
				Retryable:                []RetryableClass{RetryableUnavailable},
				FallbackOnRetryExhausted: true,
			},
		},
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	router, err := New(assembly, policy.Selectors(), options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	response, trace, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want fallback response", response.Usage)
	}
	if len(trace.Fallbacks) != 1 ||
		trace.Fallbacks[0].From.ID.Provider != "bad" ||
		trace.Fallbacks[0].To.ID.Provider != "good" {
		t.Fatalf("fallbacks = %+v", trace.Fallbacks)
	}
}

func TestFactoryBuildsRouterFromSettings(t *testing.T) {
	assembly := newRouteAssembly(t)
	factory := Factory{}
	value, err := factory.New(context.Background(), resource.Input{
		Deps: map[string]any{"target": assembly},
		Settings: []byte(`{
			"generate": [
				{"tier": "primary", "targets": [{"model": {"id": {"provider": "bad", "name": "model-1"}}}]},
				{"tier": "fallback", "targets": [{"model": {"id": {"provider": "good", "name": "model-1"}}}]}
			],
			"retry": {
				"generate": {
					"max_attempts": 1,
					"retryable": ["unavailable"],
					"fallback_on_retry_exhausted": true
				}
			}
		}`),
	})
	if err != nil {
		t.Fatalf("Factory.New: %v", err)
	}
	router, ok := value.(*Router)
	if !ok {
		t.Fatalf("Factory.New returned %T, want *Router", value)
	}
	response, _, err := router.Generate(context.Background(), routeRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want 7", response.Usage)
	}
}
