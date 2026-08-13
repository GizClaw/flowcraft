package resource_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestRegister(t *testing.T) {
	reg := resource.NewRegistry()
	if err := graphresource.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	factory, ok := reg.Lookup("agent.Engine", "graph")
	if !ok {
		t.Fatal("agent.Engine/graph factory not registered")
	}
	spec := factory.Spec()
	if spec.Kind != "agent.Engine" || spec.Impl != "graph" {
		t.Fatalf("spec = %+v", spec)
	}
	if len(spec.Deps) != 6 {
		t.Fatalf("deps = %+v, want 6", spec.Deps)
	}
}

func TestFactoryRequiresGraphSettings(t *testing.T) {
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{})
	if !errdefs.IsValidation(err) {
		t.Fatalf("New without graph = %v, want Validation", err)
	}
}

func TestFactoryRequiresToolDepForToolNode(t *testing.T) {
	def, _ := json.Marshal(map[string]any{
		"name":  "g",
		"entry": "t1",
		"nodes": []any{
			map[string]any{"id": "t1", "type": "tool", "config": map[string]any{}},
		},
		"edges": []any{},
	})
	_, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("New without tools dep = %v, want NotFound", err)
	}
}

func TestFactoryBuildsGraphEngine(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	def, _ := json.Marshal(map[string]any{
		"name":  "g",
		"entry": "n1",
		"nodes": []any{
			map[string]any{
				"id":   "n1",
				"type": "inference",
				"config": map[string]any{
					"model": map[string]any{
						"id": map[string]any{"provider": "fake", "name": "echo"},
					},
				},
			},
		},
		"edges": []any{},
	})
	value, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps: map[string]any{
			"inference": fake.Assembly(t),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(*coregraph.Graph); !ok {
		t.Fatalf("New returned %T, want *coregraph.Graph", value)
	}
}
