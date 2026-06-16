package bindings_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
)

func TestCoreCompatibility_BuildEnvWrapper(t *testing.T) {
	var fn bindings.BindingFunc = func(context.Context) (string, any) {
		return "compat", "ok"
	}

	env := bindings.BuildEnv(context.Background(), map[string]any{"k": "v"}, fn)
	if got := env.Config["k"]; got != "v" {
		t.Fatalf("config = %v, want v", got)
	}
	if got := env.Bindings["compat"]; got != "ok" {
		t.Fatalf("compat = %v, want ok", got)
	}
}

func TestCoreCompatibility_EnvBuilderAliasAndLateBinding(t *testing.T) {
	var fn bindings.BindingFunc = script.BindingFunc(func(context.Context) (string, any) {
		return "ordinary", "ok"
	})
	var late bindings.LateBindingFunc = script.LateBindingFunc(func(_ context.Context, env *script.Env) (string, any) {
		if got := env.Bindings["ordinary"]; got != "ok" {
			t.Fatalf("ordinary = %v, want ok", got)
		}
		return "late", env.Bindings
	})

	env := bindings.NewEnvBuilder(nil).Add(fn).AddLate(late).Build(context.Background())
	if got := env.Bindings["late"].(map[string]any)["ordinary"]; got != "ok" {
		t.Fatalf("late captured ordinary = %v, want ok", got)
	}
}
