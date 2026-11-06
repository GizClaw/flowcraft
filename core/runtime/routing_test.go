package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
)

// recorder is a Generator that remembers which target served the call.
type recorder struct{ served []string }

func (r *recorder) Generate(_ context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	r.served = append(r.served, request.Model.ID.Provider)
	return inference.GenerateResponse{}, nil
}

func (r *recorder) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return nil, nil
}

func routingConfig(t *testing.T, inferenceConfig InferenceConfig) (Config, *recorder) {
	t.Helper()
	cfg := localConfig(t)
	cfg.Tools.Enable = []string{ToolFiles}
	cfg.Inference = inferenceConfig
	return cfg, &recorder{}
}

// A target whose catalog does not declare the requested model is filtered out
// before it costs an attempt.
func TestRoutingFiltersByDeclaredModel(t *testing.T) {
	cfg, first := routingConfig(t, InferenceConfig{
		Providers: []ProviderConfig{
			{Name: "alpha", Driver: "first"},
			{Name: "beta", Driver: "second"},
		},
		Models: []ModelConfig{
			{Provider: "alpha", Model: "a"},
			{Provider: "beta", Model: "b"},
		},
	})
	second := &recorder{}
	rt, err := New(context.Background(), cfg, WithDrivers(
		Driver{Name: "first", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: first}, nil
		}},
		Driver{Name: "second", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: second}, nil
		}},
	))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	request := inference.GenerateRequest{Model: model.ModelRef{ID: model.ModelID{Name: "b"}}}
	if _, err := rt.Router().Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(first.served) != 0 {
		t.Errorf("alpha served %v, want it filtered out", first.served)
	}
	if len(second.served) != 1 || second.served[0] != "second" {
		t.Errorf("served = %v, want the second driver", second.served)
	}
}

// reverseSelector is a host-supplied strategy: it proves the table reaches the
// router, not just the config.
type reverseSelector struct{}

func (reverseSelector) Select(_ context.Context, _ inference.GenerateRequest, targets []route.Target) ([]route.Target, error) {
	out := append([]route.Target(nil), targets...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func TestSelectorRegistryReachesTheRouter(t *testing.T) {
	cfg, first := routingConfig(t, InferenceConfig{
		Providers: []ProviderConfig{
			{Name: "alpha", Driver: "first"},
			{Name: "beta", Driver: "second"},
		},
		Models: []ModelConfig{
			{Provider: "alpha", Model: "same"},
			{Provider: "beta", Model: "same"},
		},
	})
	cfg.Inference.Route.Selector = "reverse"
	second := &recorder{}
	rt, err := New(context.Background(), cfg,
		WithDrivers(
			Driver{Name: "first", Build: func(map[string]any) (inference.Provider, error) {
				return inference.Provider{Generator: first}, nil
			}},
			Driver{Name: "second", Build: func(map[string]any) (inference.Provider, error) {
				return inference.Provider{Generator: second}, nil
			}},
		),
		WithSelectors(Selector{
			Name: "reverse",
			Generate: func(map[string]any) (route.Selector[inference.GenerateRequest], error) {
				return reverseSelector{}, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	request := inference.GenerateRequest{Model: model.ModelRef{ID: model.ModelID{Name: "same"}}}
	if _, err := rt.Router().Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(second.served) != 1 {
		t.Fatalf("served = %v, want the reversed first candidate (beta)", second.served)
	}
	if len(first.served) != 0 {
		t.Errorf("alpha served %v despite the selector putting beta first", first.served)
	}
}

func TestUnknownSelectorIsRejected(t *testing.T) {
	cfg, _ := routingConfig(t, inferenceConfig("stub", "alpha", "a", nil))
	cfg.Inference.Route.Selector = "nope"
	_, err := New(context.Background(), cfg,
		WithDrivers(Driver{Name: "stub", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: &recorder{}}, nil
		}}),
		WithSelectors(Selector{Name: "reverse", Generate: func(map[string]any) (route.Selector[inference.GenerateRequest], error) {
			return reverseSelector{}, nil
		}}),
	)
	if err == nil {
		t.Fatal("unknown selector accepted")
	}
	if !strings.Contains(err.Error(), "reverse") {
		t.Errorf("error %q does not list the offered selectors", err)
	}
}

// driverCatalog declares one model with text input only.
type driverCatalog struct{}

func (driverCatalog) Models() []model.ModelDescriptor {
	return []model.ModelDescriptor{{
		ID:           model.ModelID{Provider: "openai", Name: "m"},
		Capabilities: model.ModelCapabilities{Inputs: []message.PartKind{message.PartText}},
	}}
}

func (c driverCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	for _, descriptor := range c.Models() {
		if descriptor.ID.Name == id.Name {
			return descriptor, true
		}
	}
	return model.ModelDescriptor{}, false
}

// A written declaration that contradicts the driver's catalog is an assembly
// error: the two describe the same fact and one of them is wrong.
func TestCatalogConflictFailsAssembly(t *testing.T) {
	withCaps := inferenceConfig("stub", "primary", "m", nil)
	withCaps.Models[0].Capabilities = &CapabilitiesConfig{Inputs: []string{"image"}}
	cfg, _ := routingConfig(t, withCaps)
	_, err := New(context.Background(), cfg, WithDrivers(
		Driver{Name: "stub", Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: &recorder{}, Catalog: driverCatalog{}}, nil
		}},
	))
	if err == nil {
		t.Fatal("contradicting capabilities accepted")
	}
	if !strings.Contains(err.Error(), "driver catalog") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}
