package script

import (
	"context"
	"reflect"
	"testing"
)

func TestBuildEnv_OrdersBindingsAndLastWins(t *testing.T) {
	var order []string
	bind := func(label, name string, value any) BindingFunc {
		return func(context.Context) (string, any) {
			order = append(order, label)
			return name, value
		}
	}
	config := map[string]any{"mode": "test"}

	env := BuildEnv(context.Background(), config,
		bind("first", "dup", "one"),
		bind("second", "keep", "value"),
		bind("third", "dup", "two"),
	)

	if !reflect.DeepEqual(order, []string{"first", "second", "third"}) {
		t.Fatalf("order = %v", order)
	}
	config["mode"] = "changed"
	if got := env.Config["mode"]; got != "changed" {
		t.Fatalf("BuildEnv should preserve the config map reference, got %v", got)
	}
	if got := env.Bindings["dup"]; got != "two" {
		t.Fatalf("dup = %v, want two", got)
	}
	if got := env.Bindings["keep"]; got != "value" {
		t.Fatalf("keep = %v, want value", got)
	}
}

func TestEnvBuilder_LateBindingsSeeAndCaptureFinalMap(t *testing.T) {
	var captured map[string]any

	env := NewEnvBuilder(nil).
		Add(
			func(context.Context) (string, any) { return "board", "parent-board" },
			func(context.Context) (string, any) { return "runtime", "ordinary-runtime" },
		).
		AddLate(
			func(_ context.Context, env *Env) (string, any) {
				if got := env.Bindings["board"]; got != "parent-board" {
					t.Fatalf("late binding did not see ordinary binding: %v", got)
				}
				captured = env.Bindings
				return "runtime", map[string]any{
					"parent": env.Bindings,
				}
			},
			func(context.Context, *Env) (string, any) {
				return "extra", "late-extra"
			},
		).
		Build(context.Background())

	runtimeBinding, ok := env.Bindings["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime binding = %T, want map[string]any", env.Bindings["runtime"])
	}
	parent := runtimeBinding["parent"].(map[string]any)
	if got, ok := parent["runtime"].(string); ok && got == "ordinary-runtime" {
		t.Fatal("late runtime binding did not overwrite ordinary runtime binding")
	}
	if _, ok := parent["runtime"].(map[string]any); !ok {
		t.Fatalf("captured parent map runtime = %T, want final runtime binding", parent["runtime"])
	}
	if got := captured["extra"]; got != "late-extra" {
		t.Fatalf("captured map did not receive later late binding: %v", got)
	}
	captured["probe"] = "same-map"
	if got := env.Bindings["probe"]; got != "same-map" {
		t.Fatalf("captured map is not env.Bindings: %v", got)
	}
}

func TestEnvBuilder_BuildReturnsIndependentBindingsMaps(t *testing.T) {
	var captures []map[string]any
	builder := NewEnvBuilder(nil).
		Add(func(context.Context) (string, any) { return "base", "value" }).
		AddLate(func(_ context.Context, env *Env) (string, any) {
			captures = append(captures, env.Bindings)
			return "runtime", env.Bindings
		})

	first := builder.Build(context.Background())
	second := builder.Build(context.Background())

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
