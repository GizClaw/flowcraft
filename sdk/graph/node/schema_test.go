package node

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func TestBuiltinSchemas_AllRegistered(t *testing.T) {
	reg := NewSchemaRegistry()
	RegisterBuiltinSchemas(reg)

	expectedTypes := []string{
		"llm", "router", "ifelse", "template",
		"answer", "assigner", "loopguard", "aggregator",
		"gate", "context", "approval", "iteration", "script",
	}

	for _, typ := range expectedTypes {
		schema, ok := reg.Get(typ)
		if !ok {
			t.Fatalf("missing schema for type %q", typ)
		}
		if schema.Label == "" {
			t.Fatalf("schema %q has empty Label", typ)
		}
		if schema.Category == "" {
			t.Fatalf("schema %q has empty Category", typ)
		}
		if schema.Description == "" {
			t.Fatalf("schema %q has empty Description", typ)
		}
	}
}

func TestBuiltinSchemas_EndNodeRegistered(t *testing.T) {
	reg := NewSchemaRegistry()
	RegisterBuiltinSchemas(reg)

	_, ok := reg.Get("__end__")
	if !ok {
		t.Fatal("expected __end__ schema")
	}
}

func TestSchemaRegistry_CRUD(t *testing.T) {
	reg := NewSchemaRegistry()

	reg.Register(NodeSchema{Type: "llm", Label: "LLM", Category: "ai"})
	reg.Register(NodeSchema{Type: "router", Label: "Router", Category: "control"})

	if reg.Len() != 2 {
		t.Fatalf("expected 2, got %d", reg.Len())
	}

	s, ok := reg.Get("llm")
	if !ok || s.Label != "LLM" {
		t.Fatalf("expected LLM schema, got %+v", s)
	}

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
	if all[0].Type != "llm" || all[1].Type != "router" {
		t.Fatal("order mismatch")
	}
}

func TestSchemaRegistry_Override(t *testing.T) {
	reg := NewSchemaRegistry()
	reg.Register(NodeSchema{Type: "llm", Label: "Old"})
	reg.Register(NodeSchema{Type: "llm", Label: "New"})

	if reg.Len() != 1 {
		t.Fatalf("expected 1 after override, got %d", reg.Len())
	}
	s, _ := reg.Get("llm")
	if s.Label != "New" {
		t.Fatalf("expected New, got %s", s.Label)
	}
}

func TestSchemaRegistry_Unregister(t *testing.T) {
	r := NewSchemaRegistry()
	r.Register(NodeSchema{Type: "a", Label: "A"})
	r.Register(NodeSchema{Type: "b", Label: "B"})
	r.Register(NodeSchema{Type: "c", Label: "C"})

	r.Unregister("b")

	if _, ok := r.Get("b"); ok {
		t.Fatal("expected 'b' to be unregistered")
	}
	if r.Len() != 2 {
		t.Fatalf("expected 2, got %d", r.Len())
	}
	all := r.All()
	if len(all) != 2 || all[0].Type != "a" || all[1].Type != "c" {
		t.Fatalf("expected [a, c], got %v", all)
	}
}

func TestSchemaRegistry_Unregister_NotExist(t *testing.T) {
	r := NewSchemaRegistry()
	r.Register(NodeSchema{Type: "a", Label: "A"})
	r.Unregister("nonexistent")
	if r.Len() != 1 {
		t.Fatalf("expected 1, got %d", r.Len())
	}
}

func TestSchemaRegistry_InputOutputPorts(t *testing.T) {
	reg := NewSchemaRegistry()
	reg.Register(NodeSchema{
		Type:  "test",
		Label: "Test",
		InputPorts: []PortSchema{
			{Name: "in", Type: "string", Required: true},
		},
		OutputPorts: []PortSchema{
			{Name: "out", Type: "string", Required: true},
		},
	})

	s, ok := reg.Get("test")
	if !ok {
		t.Fatal("expected test schema")
	}
	if len(s.InputPorts) != 1 || s.InputPorts[0].Name != "in" {
		t.Fatalf("unexpected InputPorts: %+v", s.InputPorts)
	}
	if len(s.OutputPorts) != 1 || s.OutputPorts[0].Name != "out" {
		t.Fatalf("unexpected OutputPorts: %+v", s.OutputPorts)
	}
}

func TestRegisterDefaultSchema_OverrideKeepsPortsForTypeConsistent(t *testing.T) {
	originalInput, originalOutput := PortsForType("test_override_ports")
	defer RegisterDefaultSchema(NodeSchema{
		Type:        "test_override_ports",
		Label:       "original",
		InputPorts:  toPortSchemas(originalInput),
		OutputPorts: toPortSchemas(originalOutput),
	})

	RegisterDefaultSchema(NodeSchema{
		Type:       "test_override_ports",
		Label:      "old",
		InputPorts: []PortSchema{{Name: "old_in", Type: "string"}},
	})
	RegisterDefaultSchema(NodeSchema{
		Type:        "test_override_ports",
		Label:       "new",
		InputPorts:  []PortSchema{{Name: "new_in", Type: "number"}},
		OutputPorts: []PortSchema{{Name: "new_out", Type: "boolean"}},
	})

	reg := NewSchemaRegistry()
	RegisterBuiltinSchemas(reg)
	schema, ok := reg.Get("test_override_ports")
	if !ok {
		t.Fatal("expected overridden schema to be registered")
	}
	input, output := PortsForType("test_override_ports")
	if len(input) != 1 || input[0].Name != schema.InputPorts[0].Name {
		t.Fatalf("input ports mismatch: registry=%+v portsForType=%+v", schema.InputPorts, input)
	}
	if len(output) != 1 || output[0].Name != schema.OutputPorts[0].Name {
		t.Fatalf("output ports mismatch: registry=%+v portsForType=%+v", schema.OutputPorts, output)
	}
}

func toPortSchemas(ports []graph.Port) []PortSchema {
	if len(ports) == 0 {
		return nil
	}
	out := make([]PortSchema, len(ports))
	for i, p := range ports {
		out[i] = PortSchema{
			Name:        p.Name,
			Type:        string(p.Type),
			Required:    p.Required,
			Description: p.Desc,
		}
	}
	return out
}
