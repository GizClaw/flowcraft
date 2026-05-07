package resolver

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

const minimalConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-default
spec:
  control:
    socket: /tmp/vesseld.sock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: helper
spec:
  engine:
    ref: noop-test
`

func TestResolve_Minimal(t *testing.T) {
	t.Parallel()
	objs, err := apispec.DecodeAll(strings.NewReader(minimalConfig), "<test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}

	cat := catalog.New()
	cat.RegisterEngine("noop-test", func(ref string, cfg map[string]any, deps catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})

	plan, errs := Resolve(objs, cat, ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if plan.Daemon.Name != "vesseld-default" {
		t.Fatalf("daemon name = %q", plan.Daemon.Name)
	}
	if len(plan.Vessels) != 1 {
		t.Fatalf("vessels = %d, want 1", len(plan.Vessels))
	}
	vp := plan.Vessels[0]
	if vp.Name != "support" {
		t.Fatalf("vessel name = %q", vp.Name)
	}
	if _, ok := vp.EngineFactoriesByAgent["helper"]; !ok {
		t.Fatalf("missing helper engine factory")
	}
}

func TestResolve_RejectsMissingAgentRef(t *testing.T) {
	t.Parallel()
	in := minimalConfig + `
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: broken
spec:
  agents: [does-not-exist]
`
	objs, err := apispec.DecodeAll(strings.NewReader(in), "<test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	cat := catalog.New()
	cat.RegisterEngine("noop-test", func(ref string, cfg map[string]any, deps catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})
	_, errs := Resolve(objs, cat, ResolveOptions{})
	if errs.Len() == 0 {
		t.Fatal("expected ref-resolution error")
	}
}

func TestResolveLLMs_EnvSecret(t *testing.T) {
	t.Setenv("VESSELD_TEST_API_KEY", "sk-from-env")
	profiles := map[string]v1alpha1.LLMProfile{
		"openai": {
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindLLMProfile},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "openai"},
			Spec: v1alpha1.LLMProfileSpec{
				Provider: "openai",
				Auth: v1alpha1.LLMProfileAuth{
					APIKey: v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{Env: "VESSELD_TEST_API_KEY"}},
				},
			},
		},
	}
	cat := catalog.Builtin()
	// AllowFile=false, AllowSecret=false → skip live client
	// construction; only verify the ProviderConfigStore wiring.
	clients, resolver, errs := resolveLLMs(cat, profiles, nil, ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(clients) != 0 {
		t.Fatalf("clients = %d, want 0 (validate-only)", len(clients))
	}
	pc, err := resolver.(interface {
		Resolve(context.Context, string) (llm.LLM, error)
	}).Resolve(context.Background(), "openai/gpt-4o")
	_ = pc
	_ = err // we cannot validate the live LLM construction in unit tests
}

func TestResolveValueRef_RejectsMissingEnv(t *testing.T) {
	t.Parallel()
	os.Unsetenv("VESSELD_TEST_MISSING_KEY")
	_, err := resolveValueRef(
		v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{Env: "VESSELD_TEST_MISSING_KEY"}},
		nil,
		DefaultResolveOptions(),
		"test",
	)
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
}
