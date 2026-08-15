package resource_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	graphresource "github.com/GizClaw/flowcraft/core/graph/resource"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
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

// graphTestExtension is a canned provider extension riding the fake
// provider definition, proving graph yaml extensions reach the runtime.
type graphTestExtension struct {
	Provider string
	Flag     bool `json:"flag,omitempty"`
}

func (e *graphTestExtension) ProviderID() string  { return e.Provider }
func (e *graphTestExtension) ExtensionID() string { return "generate_options" }
func (e *graphTestExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"flag"}
}
func (e *graphTestExtension) Validate() error { return nil }
func (e *graphTestExtension) Clone() inference.Extension {
	copy := *e
	return &copy
}

func graphWithExtensions(t *testing.T, model any) []byte {
	t.Helper()
	node := map[string]any{
		"id":   "n1",
		"type": "inference",
		"config": map[string]any{
			"extensions": []any{
				map[string]any{
					"provider": "fake",
					"id":       "generate_options",
					"fields":   map[string]any{"flag": true},
				},
			},
		},
	}
	if model != nil {
		node["config"].(map[string]any)["model"] = model
	}
	def, err := json.Marshal(map[string]any{
		"name":  "g",
		"entry": "n1",
		"nodes": []any{node},
		"edges": []any{},
	})
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	return def
}

func executeTestGraph(t *testing.T, g *coregraph.Graph) error {
	t.Helper()
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "hi"))
	_, err := g.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		agent.NoopHost{}, board)
	return err
}

func fakeWithDecoder(t *testing.T) *inferencetest.GenerateFake {
	t.Helper()
	return &inferencetest.GenerateFake{
		ExtensionDecoders: map[string]inference.ExtensionDecoder{
			"generate_options": inference.ExtensionDecoderFor(func() *graphTestExtension {
				return &graphTestExtension{Provider: "fake"}
			}),
		},
	}
}

func TestFactoryWiresProviderCarriedInferenceExtensions(t *testing.T) {
	fake := fakeWithDecoder(t)
	def := graphWithExtensions(t, map[string]any{
		"id":      map[string]any{"provider": "fake", "name": "echo"},
		"profile": "default",
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
	if err := executeTestGraph(t, value.(*coregraph.Graph)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := fake.LastRequest()
	if len(req.Extensions) != 1 {
		t.Fatalf("request extensions = %#v, want 1", req.Extensions)
	}
	ext, ok := req.Extensions[0].(*graphTestExtension)
	if !ok || !ext.Flag || ext.Provider != "fake" {
		t.Fatalf("decoded extension = %#v", req.Extensions[0])
	}
}

func TestFactoryWiresExtensionsForRouterOnlyDeployment(t *testing.T) {
	fake := fakeWithDecoder(t)
	router, err := route.New(fake.Assembly(t), route.Selectors{
		Generate: inferencetest.StaticGenerateSelector(inferencetest.DefaultFakeModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	value, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(graphWithExtensions(t, nil)) + `}`),
		Deps: map[string]any{
			"router": router,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := executeTestGraph(t, value.(*coregraph.Graph)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fake.LastRequest().Extensions) != 1 {
		t.Fatalf("request extensions = %#v, want 1", fake.LastRequest().Extensions)
	}
}

func TestFactoryRejectsExtensionsWithoutConfiguredDecoder(t *testing.T) {
	def := graphWithExtensions(t, map[string]any{
		"id": map[string]any{"provider": "fake", "name": "echo"},
	})
	value, err := (graphresource.Factory{}).New(context.Background(), resource.Input{
		Settings: []byte(`{"graph": ` + string(def) + `}`),
		Deps: map[string]any{
			"inference": (&inferencetest.GenerateFake{}).Assembly(t),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = executeTestGraph(t, value.(*coregraph.Graph))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Execute error = %v, want Validation for unregistered extension", err)
	}
}
