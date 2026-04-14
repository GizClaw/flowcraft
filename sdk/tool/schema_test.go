package tool

import "testing"

func TestSchemaBuilder_Basic(t *testing.T) {
	def := DefineSchema("my_tool", "does things",
		Property("name", "string", "the name"),
		Property("count", "integer", "the count"),
	).Required("name").Build()

	if def.Name != "my_tool" {
		t.Fatalf("Name = %q, want %q", def.Name, "my_tool")
	}
	if def.Description != "does things" {
		t.Fatalf("Description = %q, want %q", def.Description, "does things")
	}

	props, ok := def.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("InputSchema missing properties")
	}
	if _, ok := props["name"]; !ok {
		t.Fatal("missing property 'name'")
	}
	if _, ok := props["count"]; !ok {
		t.Fatal("missing property 'count'")
	}

	req, ok := def.InputSchema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "name" {
		t.Fatalf("required = %v, want [name]", req)
	}
}

func TestSchemaBuilder_NoRequired(t *testing.T) {
	def := DefineSchema("test", "test tool",
		Property("x", "string", "x"),
	).Build()

	if _, ok := def.InputSchema["required"]; ok {
		t.Fatal("should not have required when none specified")
	}
}

func TestSchemaBuilder_Empty(t *testing.T) {
	def := DefineSchema("empty", "no params").Build()
	if def.Name != "empty" {
		t.Fatalf("Name = %q", def.Name)
	}
	props := def.InputSchema["properties"].(map[string]any)
	if len(props) != 0 {
		t.Fatalf("expected empty properties, got %d", len(props))
	}
}

func TestArrayProperty(t *testing.T) {
	prop := ArrayProperty("tags", "list of tags", map[string]any{"type": "string"})
	if prop.name != "tags" {
		t.Fatalf("name = %q", prop.name)
	}
	if prop.schema["type"] != "array" {
		t.Fatalf("type = %v", prop.schema["type"])
	}
}

func TestEnumProperty(t *testing.T) {
	prop := EnumProperty("mode", "string", "operation mode", "read", "write")
	if prop.name != "mode" {
		t.Fatalf("name = %q", prop.name)
	}
	enums, ok := prop.schema["enum"].([]any)
	if !ok || len(enums) != 2 {
		t.Fatalf("enum = %v", prop.schema["enum"])
	}
}

func TestObjectProperty(t *testing.T) {
	prop := ObjectProperty("address", "mailing address", map[string]any{
		"street": map[string]any{"type": "string"},
		"city":   map[string]any{"type": "string"},
	})
	if prop.name != "address" {
		t.Fatalf("name = %q", prop.name)
	}
	if prop.schema["type"] != "object" {
		t.Fatalf("type = %v", prop.schema["type"])
	}
	if prop.schema["description"] != "mailing address" {
		t.Fatalf("description = %v", prop.schema["description"])
	}
	props, ok := prop.schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if len(props) != 2 {
		t.Fatalf("properties count = %d, want 2", len(props))
	}
}

func TestObjectProperty_EmptyProperties(t *testing.T) {
	prop := ObjectProperty("empty", "empty obj", nil)
	if _, ok := prop.schema["properties"]; ok {
		t.Error("nil properties should not produce a 'properties' key")
	}
}

func TestSchemaBuilder_MultipleRequired(t *testing.T) {
	def := DefineSchema("t", "d",
		Property("a", "string", "a"),
		Property("b", "string", "b"),
		Property("c", "string", "c"),
	).Required("a", "b").Required("c").Build()

	req, ok := def.InputSchema["required"].([]string)
	if !ok {
		t.Fatal("missing required")
	}
	if len(req) != 3 {
		t.Fatalf("required count = %d, want 3", len(req))
	}
}

func TestDefineSchema_WithAllPropertyTypes(t *testing.T) {
	def := DefineSchema("full", "all types",
		Property("name", "string", "a name"),
		ArrayProperty("tags", "tags", map[string]any{"type": "string"}),
		ObjectProperty("meta", "metadata", map[string]any{
			"key": map[string]any{"type": "string"},
		}),
		EnumProperty("status", "string", "status", "active", "inactive"),
	).Required("name").Build()

	props := def.InputSchema["properties"].(map[string]any)
	if len(props) != 4 {
		t.Fatalf("properties count = %d, want 4", len(props))
	}
}
