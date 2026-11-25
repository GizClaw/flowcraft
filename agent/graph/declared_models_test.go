package graph

import (
	"context"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/model"
	"github.com/goccy/go-json"
)

// namingKind is a node kind whose config names models, which is what the model kind does. The
// engine must not need to know the kind's name to collect them.
type namingKind struct{}

func (namingKind) Name() string { return "naming" }

func (namingKind) DecodeConfig(raw json.RawMessage) (any, error) {
	var config namingConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	return config, nil
}

func (namingKind) Invoke(context.Context, Invoke) (NodeStream, error) { return Static(Result{}), nil }

// namingConfig declares one model, or none when it names no model.
type namingConfig struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

func (c namingConfig) DeclaredModels() []model.ModelRef {
	if c.Name == "" {
		return nil
	}
	return []model.ModelRef{{ID: model.ModelID{Provider: c.Provider, Name: c.Name}}}
}

// TestDeclaredModelsReadsEveryKindsConfig is the static half of a host's model check: the graph
// reports what its nodes say they will call, without the engine knowing which kinds call a model.
func TestDeclaredModelsReadsEveryKindsConfig(t *testing.T) {
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: naming, config: {provider: openai, name: gpt-4o}}
  - {id: b, kind: naming, config: {provider: openai, name: gpt-4o}}
  - {id: c, kind: naming, config: {provider: openai, name: gpt-4o-mini}}
  - {id: d, kind: count}
edges:
  - {from: a, to: b}
  - {from: b, to: c}
  - {from: c, to: d}
  - {from: d, to: end}
`, &namingKind{}, &countKind{})

	declared := g.DeclaredModels()
	want := []model.ModelRef{
		{ID: model.ModelID{Provider: "openai", Name: "gpt-4o"}},
		{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}},
	}
	if !slices.Equal(declared, want) {
		t.Fatalf("DeclaredModels = %v, want %v in declaration order", declared, want)
	}
}

// TestDeclaredModelsAddsTheDeploymentDefault: a node that names no model calls the deployment's
// default, so the program's surface includes it.
func TestDeclaredModelsAddsTheDeploymentDefault(t *testing.T) {
	g := build(t, `
version: 1
entry: a
nodes:
  - {id: a, kind: naming, config: {provider: openai, name: gpt-4o}}
  - {id: b, kind: naming, config: {}}
edges:
  - {from: a, to: b}
  - {from: b, to: end}
`, &namingKind{})

	def := model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-5"}}
	program := NewProgram(g, WithProgramDefaultModel(def))
	declared := program.DeclaredModels()
	if !slices.Contains(declared, def) {
		t.Fatalf("DeclaredModels = %v, want the default %v", declared, def)
	}
	if len(declared) != 2 {
		t.Fatalf("DeclaredModels = %v, want the node's model and the default", declared)
	}
}
