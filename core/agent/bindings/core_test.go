package bindings

import (
	"context"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// mustBuildEnv assembles a script env for tests.
func mustBuildEnv(t testing.TB, config map[string]any, fns ...BindingFunc) *agent.ScriptEnv {
	t.Helper()
	env, err := BuildEnv(context.Background(), config, fns...)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	return env
}

func TestBuildEnv_OrdersBindingsAndPreservesConfig(t *testing.T) {
	var order []string
	bind := func(label, name string, value any) BindingFunc {
		return func(context.Context) (string, any) {
			order = append(order, label)
			return name, value
		}
	}
	config := map[string]any{"mode": "test"}

	env := mustBuildEnv(t, config,
		bind("first", "board", "one"),
		bind("second", "keep", "value"),
		bind("third", "tools", "two"),
	)

	if !reflect.DeepEqual(order, []string{"first", "second", "third"}) {
		t.Fatalf("order = %v", order)
	}
	config["mode"] = "changed"
	if got := env.Config["mode"]; got != "changed" {
		t.Fatalf("BuildEnv should preserve the config map reference, got %v", got)
	}
	if got := env.Bindings["board"]; got != "one" {
		t.Fatalf("board = %v, want one", got)
	}
	if got := env.Bindings["keep"]; got != "value" {
		t.Fatalf("keep = %v, want value", got)
	}
}

func TestBuildEnv_RejectsDuplicateNames(t *testing.T) {
	bind := func(name string) BindingFunc {
		return func(context.Context) (string, any) { return name, "value" }
	}
	_, err := BuildEnv(context.Background(), nil, bind("dup"), bind("dup"))
	if !errdefs.IsValidation(err) {
		t.Fatalf("BuildEnv = %v, want Validation for a duplicate global", err)
	}
}

func TestBuilder_LateBindingsSeeAndCaptureFinalMap(t *testing.T) {
	var captured map[string]any

	env, err := Assemble(Invocation{Context: context.Background()}, nil,
		NewBuilder().
			Add(func(context.Context) (string, any) { return "board", "parent-board" }).
			AddLate(func(_ context.Context, env *agent.ScriptEnv) (string, any) {
				if got := env.Bindings["board"]; got != "parent-board" {
					t.Fatalf("late binding did not see ordinary binding: %v", got)
				}
				captured = env.Bindings
				return "runtime", map[string]any{"parent": env.Bindings}
			}).
			AddLate(func(context.Context, *agent.ScriptEnv) (string, any) {
				return "extra", "late-extra"
			}))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	runtimeBinding, ok := env.Bindings["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime binding = %T, want map[string]any", env.Bindings["runtime"])
	}
	if got := runtimeBinding["parent"]; !reflect.DeepEqual(got, captured) {
		t.Fatalf("captured parent map = %v, want the env bindings", got)
	}
	if got := captured["extra"]; got != "late-extra" {
		t.Fatalf("captured map did not receive later late binding: %v", got)
	}
	captured["probe"] = "same-map"
	if got := env.Bindings["probe"]; got != "same-map" {
		t.Fatalf("captured map is not env.Bindings: %v", got)
	}
}

func TestAssemble_ReturnsIndependentBindingsMaps(t *testing.T) {
	var captures []map[string]any
	builder := NewBuilder().
		Add(func(context.Context) (string, any) { return "base", "value" }).
		AddLate(func(_ context.Context, env *agent.ScriptEnv) (string, any) {
			captures = append(captures, env.Bindings)
			return "runtime", env.Bindings
		})

	first, err := Assemble(Invocation{Context: context.Background()}, nil, builder)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	second, err := Assemble(Invocation{Context: context.Background()}, nil, builder)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	first.Bindings["only_first"] = true
	captures[0]["captured_first"] = true
	if _, ok := second.Bindings["only_first"]; ok {
		t.Fatal("second build saw mutation from first build")
	}
	if _, ok := second.Bindings["captured_first"]; ok {
		t.Fatal("second captured map saw mutation from first captured map")
	}
	second.Bindings["only_second"] = true
	if _, ok := first.Bindings["only_second"]; ok {
		t.Fatal("first build saw mutation from second build")
	}
}
