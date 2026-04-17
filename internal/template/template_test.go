package template

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/internal/model"
)

func TestRegistry_RegisterBuiltins(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	expectedNames := []string{
		"blank", "react_agent",
		"copilot_dispatcher", "copilot_builder",
	}

	all := reg.All()
	if len(all) != len(expectedNames) {
		t.Fatalf("All() len = %d, want %d", len(all), len(expectedNames))
	}

	for _, name := range expectedNames {
		tmpl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("missing template %q", name)
		}
		if tmpl.Label == "" {
			t.Fatalf("template %q has empty Label", name)
		}
		if tmpl.Description == "" {
			t.Fatalf("template %q has empty Description", name)
		}
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	reg := NewRegistry()
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Fatal("Get should return false for unregistered template")
	}
}

func TestInstantiate_ReactAgent(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	tmpl, _ := reg.Get("react_agent")
	result, err := Instantiate(tmpl, map[string]any{
		"system_prompt": "You are a pirate.",
	})
	if err != nil {
		t.Fatalf("Instantiate error: %v", err)
	}

	if result["entry"] != "llm_call" {
		t.Fatalf("entry = %v", result["entry"])
	}

	nodes, ok := result["nodes"].([]any)
	if !ok {
		t.Fatal("nodes should be an array")
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes len = %d, want 3", len(nodes))
	}
}

func TestInstantiate_DefaultValues(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	tmpl, _ := reg.Get("react_agent")
	result, err := Instantiate(tmpl, map[string]any{})
	if err != nil {
		t.Fatalf("Instantiate error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
}

func TestInstantiate_BlankTemplate(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	tmpl, ok := reg.Get("blank")
	if !ok {
		t.Fatal("blank template not found")
	}

	result, err := Instantiate(tmpl, map[string]any{})
	if err != nil {
		t.Fatalf("Instantiate error: %v", err)
	}

	nodes, ok := result["nodes"].([]any)
	if !ok {
		t.Fatal("nodes should be an array")
	}
	if len(nodes) != 0 {
		t.Fatalf("blank template nodes len = %d, want 0", len(nodes))
	}
}

func TestRegistry_Save_PersistsToStore(t *testing.T) {
	store := &memTemplateStore{templates: make(map[string]*model.Template)}
	reg := NewRegistry()
	reg.SetStore(store)

	tmpl := GraphTemplate{
		Name:        "custom_flow",
		Label:       "Custom Flow",
		Description: "A custom workflow template",
		Category:    "custom",
		GraphDef:    map[string]any{"entry": "start", "nodes": []any{}, "edges": []any{}},
	}
	if err := reg.Save(context.Background(), tmpl); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	got, ok := reg.Get("custom_flow")
	if !ok {
		t.Fatal("template not in cache after Save")
	}
	if got.Label != "Custom Flow" {
		t.Fatalf("Label = %q, want %q", got.Label, "Custom Flow")
	}

	if _, ok := store.templates["custom_flow"]; !ok {
		t.Fatal("template not persisted to store")
	}
}

func TestRegistry_Save_WithoutStore(t *testing.T) {
	reg := NewRegistry()
	tmpl := GraphTemplate{
		Name:     "ephemeral",
		Label:    "Ephemeral",
		GraphDef: map[string]any{"entry": ""},
	}
	if err := reg.Save(context.Background(), tmpl); err != nil {
		t.Fatalf("Save without store should succeed: %v", err)
	}
	if _, ok := reg.Get("ephemeral"); !ok {
		t.Fatal("template should be in cache")
	}
}

func TestRegistry_Delete_UserTemplate(t *testing.T) {
	store := &memTemplateStore{templates: make(map[string]*model.Template)}
	reg := NewRegistry()
	reg.SetStore(store)

	tmpl := GraphTemplate{
		Name:     "deletable",
		Label:    "Deletable",
		GraphDef: map[string]any{"entry": ""},
	}
	_ = reg.Save(context.Background(), tmpl)

	if err := reg.Delete(context.Background(), "deletable"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, ok := reg.Get("deletable"); ok {
		t.Fatal("template should be removed from cache after delete")
	}
}

func TestRegistry_Delete_BuiltinRejected(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	err := reg.Delete(context.Background(), "blank")
	if err == nil {
		t.Fatal("expected error deleting built-in template")
	}
	if _, ok := reg.Get("blank"); !ok {
		t.Fatal("built-in template should still exist")
	}
}

func TestRegistry_Delete_NotFound(t *testing.T) {
	reg := NewRegistry()
	err := reg.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error deleting nonexistent template")
	}
}

func TestRegistry_LoadFromStore_RestoresUserTemplates(t *testing.T) {
	store := &memTemplateStore{templates: map[string]*model.Template{
		"user_flow": {
			Name:        "user_flow",
			Label:       "User Flow",
			Description: "from database",
			Category:    "user",
			Parameters:  "[]",
			GraphDef:    `{"entry":"start","nodes":[],"edges":[]}`,
		},
	}}

	reg := NewRegistry()
	reg.RegisterBuiltins()
	reg.SetStore(store)

	if err := reg.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("LoadFromStore error: %v", err)
	}

	got, ok := reg.Get("user_flow")
	if !ok {
		t.Fatal("user template not loaded from store")
	}
	if got.Label != "User Flow" {
		t.Fatalf("Label = %q, want %q", got.Label, "User Flow")
	}
	if got.IsBuiltin {
		t.Fatal("user template should not be marked as builtin")
	}

	if _, ok := reg.Get("blank"); !ok {
		t.Fatal("built-in template should still exist")
	}
}

func TestRegistry_LoadFromStore_SyncsBuiltins(t *testing.T) {
	store := &memTemplateStore{templates: make(map[string]*model.Template)}
	reg := NewRegistry()
	reg.RegisterBuiltins()
	reg.SetStore(store)

	if err := reg.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("LoadFromStore error: %v", err)
	}

	if _, ok := store.templates["blank"]; !ok {
		t.Fatal("built-in template 'blank' should be synced to store")
	}
	if _, ok := store.templates["react_agent"]; !ok {
		t.Fatal("built-in template 'react_agent' should be synced to store")
	}
}

func TestRegistry_RegisterBuiltins_SetsIsBuiltin(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterBuiltins()

	for _, name := range []string{"blank", "react_agent", "copilot_dispatcher", "copilot_builder"} {
		tmpl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("missing template %q", name)
		}
		if !tmpl.IsBuiltin {
			t.Fatalf("template %q should have IsBuiltin=true", name)
		}
	}
}

func TestModelConversion_RoundTrip(t *testing.T) {
	original := GraphTemplate{
		Name:        "round_trip",
		Label:       "Round Trip",
		Description: "test round trip",
		Category:    "test",
		Parameters: []TemplateParameter{
			{Name: "prompt", Label: "Prompt", Type: "string", Required: true},
		},
		GraphDef:  map[string]any{"entry": "start", "nodes": []any{}},
		IsBuiltin: false,
	}

	mt, err := toModel(original)
	if err != nil {
		t.Fatalf("toModel error: %v", err)
	}

	restored, err := fromModel(mt)
	if err != nil {
		t.Fatalf("fromModel error: %v", err)
	}

	if restored.Name != original.Name {
		t.Fatalf("Name = %q, want %q", restored.Name, original.Name)
	}
	if restored.Label != original.Label {
		t.Fatalf("Label = %q, want %q", restored.Label, original.Label)
	}
	if len(restored.Parameters) != 1 {
		t.Fatalf("Parameters len = %d, want 1", len(restored.Parameters))
	}
	if restored.Parameters[0].Name != "prompt" {
		t.Fatalf("Parameters[0].Name = %q, want %q", restored.Parameters[0].Name, "prompt")
	}
}

// memTemplateStore is a minimal in-memory model.Store-like implementation
// used only for template tests.
type memTemplateStore struct {
	templates map[string]*model.Template
}

func (s *memTemplateStore) ListTemplates(_ context.Context) ([]*model.Template, error) {
	out := make([]*model.Template, 0, len(s.templates))
	for _, t := range s.templates {
		out = append(out, t)
	}
	return out, nil
}

func (s *memTemplateStore) SaveTemplate(_ context.Context, t *model.Template) error {
	s.templates[t.Name] = t
	return nil
}

func (s *memTemplateStore) DeleteTemplate(_ context.Context, name string) error {
	delete(s.templates, name)
	return nil
}

func TestCoPilotReferenceDocs(t *testing.T) {
	docs := CoPilotReferenceDocs()
	if len(docs) != 3 {
		t.Fatalf("CoPilotReferenceDocs len = %d, want 3", len(docs))
	}

	expected := map[string]bool{
		"topology-patterns": false,
		"common-pitfalls":   false,
		"memory-tools":      false,
	}
	for _, doc := range docs {
		if _, ok := expected[doc.Name]; !ok {
			t.Fatalf("unexpected doc name %q", doc.Name)
		}
		expected[doc.Name] = true
		if doc.Content == "" {
			t.Fatalf("doc %q has empty content", doc.Name)
		}
	}
	for name, found := range expected {
		if !found {
			t.Fatalf("missing reference doc %q", name)
		}
	}
}
