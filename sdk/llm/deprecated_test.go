package llm

import (
	"context"
	"testing"
)

// Coverage for the alias shims in deprecated.go: they must remain
// behaviorally equivalent to their canonical replacements until the
// v0.3.0 cutover removes them.

func TestDeprecated_CapsMiddleware_EquivalentToWithCaps(t *testing.T) {
	inner := &capsMockLLM{}
	wrapped := CapsMiddleware(inner, DisabledCaps(CapTemperature))
	_, _, _ = wrapped.Generate(context.Background(), nil, WithTemperature(0.5))
	if inner.lastOpts.Temperature != nil {
		t.Fatal("CapsMiddleware alias must behave like WithCaps")
	}
}

func TestDeprecated_LookupModelCaps_EquivalentToSpecCaps(t *testing.T) {
	reg := NewProviderRegistry()
	reg.RegisterModels("p", []ModelInfo{
		{Name: "m", Spec: ModelSpec{Caps: DisabledCaps(CapJSONMode)}},
	})
	if reg.LookupModelCaps("p", "m").Supports(CapJSONMode) {
		t.Fatal("LookupModelCaps alias must surface Spec.Caps")
	}
}

func TestDeprecated_WithExtraCaps_EquivalentToWithPolicyCaps(t *testing.T) {
	store := newResolverMockStore()
	reg := NewProviderRegistry()
	reg.Register("p", func(model string, _ map[string]any) (LLM, error) {
		return &capsMockLLM{}, nil
	})
	store.configs["p"] = &ProviderConfig{Provider: "p", Config: map[string]any{}}

	r := newResolverWithRegistry(store, reg, WithExtraCaps(DisabledCaps(CapTemperature)))
	inst, err := r.Resolve(context.Background(), "p/m")
	if err != nil {
		t.Fatal(err)
	}
	mock := unwrapMock(inst)
	_, _, _ = inst.Generate(context.Background(), nil, WithTemperature(0.7))
	if mock.lastOpts.Temperature != nil {
		t.Fatal("WithExtraCaps alias must behave like WithPolicyCaps")
	}
}

// unwrapMock walks the middleware chain to the inner *capsMockLLM
// (Defaults / Caps / Limits wrappers may be present in any order
// dictated by the resolver's one-shot assembly).
func unwrapMock(l LLM) *capsMockLLM {
	for {
		switch v := l.(type) {
		case *capsMockLLM:
			return v
		case *defaultsLLM:
			l = v.inner
		case *capsLLM:
			l = v.inner
		case *limitsLLM:
			l = v.inner
		default:
			return nil
		}
	}
}
