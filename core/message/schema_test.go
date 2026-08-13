package message

import (
	"encoding/json"
	"testing"
)

// schemaMap decodes a built ToolDefinition's InputSchema (JSON) into a map
// so the assertions below can inspect the JSON Schema structure.
func schemaMap(t *testing.T, def ToolDefinition) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(def.InputSchema, &m); err != nil {
		t.Fatalf("InputSchema not a JSON object: %v", err)
	}
	return m
}

func TestSchemaBuilder_Basic(t *testing.T) {
	def := DefineSchema("my_tool", "does things",
		ToolProperty("name", "string", "the name"),
		ToolProperty("count", "integer", "the count"),
	).Required("name").Build()

	if def.Name != "my_tool" {
		t.Fatalf("Name = %q, want %q", def.Name, "my_tool")
	}
	if def.Description != "does things" {
		t.Fatalf("Description = %q, want %q", def.Description, "does things")
	}

	schema := schemaMap(t, def)
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("InputSchema missing properties")
	}
	if _, ok := props["name"]; !ok {
		t.Fatal("missing property 'name'")
	}
	if _, ok := props["count"]; !ok {
		t.Fatal("missing property 'count'")
	}

	req, ok := schema["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "name" {
		t.Fatalf("required = %v, want [name]", req)
	}
}

func TestSchemaBuilder_NoRequired(t *testing.T) {
	def := DefineSchema("test", "test tool",
		ToolProperty("x", "string", "x"),
	).Build()

	if _, ok := schemaMap(t, def)["required"]; ok {
		t.Fatal("should not have required when none specified")
	}
}

func TestSchemaBuilder_Empty(t *testing.T) {
	def := DefineSchema("empty", "no params").Build()
	if def.Name != "empty" {
		t.Fatalf("Name = %q", def.Name)
	}
	props := schemaMap(t, def)["properties"].(map[string]any)
	if len(props) != 0 {
		t.Fatalf("expected empty properties, got %d", len(props))
	}
}

func TestArrayProperty(t *testing.T) {
	prop := ToolArrayProperty("tags", "list of tags", map[string]any{"type": "string"})
	schema := prop.schema
	if schema["type"] != "array" {
		t.Fatalf("type = %v", schema["type"])
	}
}

func TestEnumProperty(t *testing.T) {
	prop := ToolEnumProperty("mode", "string", "operation mode", "read", "write")
	schema := prop.schema
	enums, ok := schema["enum"].([]any)
	if !ok || len(enums) != 2 {
		t.Fatalf("enum = %v", schema["enum"])
	}
}

func TestObjectProperty(t *testing.T) {
	prop := ToolObjectProperty("address", "mailing address", map[string]any{
		"street": map[string]any{"type": "string"},
		"city":   map[string]any{"type": "string"},
	})
	schema := prop.schema
	if schema["type"] != "object" {
		t.Fatalf("type = %v", schema["type"])
	}
	if schema["description"] != "mailing address" {
		t.Fatalf("description = %v", schema["description"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if len(props) != 2 {
		t.Fatalf("properties count = %d, want 2", len(props))
	}
}

func TestObjectProperty_EmptyProperties(t *testing.T) {
	prop := ToolObjectProperty("empty", "empty obj", nil)
	if _, ok := prop.schema["properties"]; ok {
		t.Error("nil properties should not produce a 'properties' key")
	}
}

func TestStringMapProperty(t *testing.T) {
	def := DefineSchema("metadata", "accepts metadata",
		ToolStringMapProperty("metadata", "string metadata"),
	).Build()
	props := schemaMap(t, def)["properties"].(map[string]any)
	metadata := props["metadata"].(map[string]any)
	if metadata["type"] != "object" {
		t.Fatalf("type = %v, want object", metadata["type"])
	}
	if metadata["description"] != "string metadata" {
		t.Fatalf("description = %v, want string metadata", metadata["description"])
	}
	additional := metadata["additionalProperties"].(map[string]any)
	if additional["type"] != "string" {
		t.Fatalf("additionalProperties.type = %v, want string", additional["type"])
	}
}

func TestSchemaBuilder_MultipleRequired(t *testing.T) {
	def := DefineSchema("t", "d",
		ToolProperty("a", "string", "a"),
		ToolProperty("b", "string", "b"),
		ToolProperty("c", "string", "c"),
	).Required("a", "b").Required("c").Build()

	req, ok := schemaMap(t, def)["required"].([]any)
	if !ok {
		t.Fatal("missing required")
	}
	if len(req) != 3 {
		t.Fatalf("required count = %d, want 3", len(req))
	}
}

func TestDefineSchema_WithAllPropertyTypes(t *testing.T) {
	def := DefineSchema("full", "all types",
		ToolProperty("name", "string", "a name"),
		ToolArrayProperty("tags", "tags", map[string]any{"type": "string"}),
		ToolObjectProperty("meta", "metadata", map[string]any{
			"key": map[string]any{"type": "string"},
		}),
		ToolEnumProperty("status", "string", "status", "active", "inactive"),
	).Required("name").Build()

	props := schemaMap(t, def)["properties"].(map[string]any)
	if len(props) != 4 {
		t.Fatalf("properties count = %d, want 4", len(props))
	}
}

func TestPropertyWithDefault(t *testing.T) {
	prop := ToolPropertyWithDefault("limit", "integer", "max items", 10)
	schema := prop.schema
	if schema["type"] != "integer" {
		t.Fatalf("type = %v", schema["type"])
	}
	if schema["default"] != 10 {
		t.Fatalf("default = %v, want 10", schema["default"])
	}

	def := DefineSchema("t", "d",
		ToolPropertyWithDefault("enabled", "boolean", "toggle", true),
	).Build()
	props := schemaMap(t, def)["properties"].(map[string]any)
	enabled := props["enabled"].(map[string]any)
	if enabled["default"] != true {
		t.Fatalf("default = %v, want true", enabled["default"])
	}
}

func TestRequired_Dedup(t *testing.T) {
	def := DefineSchema("t", "d",
		ToolProperty("a", "string", "a"),
		ToolProperty("b", "string", "b"),
	).Required("a", "b").Required("a").Required("b", "a").Build()

	req := schemaMap(t, def)["required"].([]any)
	if len(req) != 2 {
		t.Fatalf("required count = %d, want 2 (deduped)", len(req))
	}
}

func TestItems(t *testing.T) {
	items := Items("string")
	if items["type"] != "string" {
		t.Fatalf("Items(string) = %v", items)
	}
	def := DefineSchema("t", "d",
		ToolArrayProperty("args", "arguments", Items("string")),
	).Build()
	props := schemaMap(t, def)["properties"].(map[string]any)
	args := props["args"].(map[string]any)
	got := args["items"].(map[string]any)
	if got["type"] != "string" {
		t.Fatalf("args.items.type = %v, want string", got["type"])
	}
}

func TestDisallowAdditionalProperties(t *testing.T) {
	def := DefineSchema("t", "d",
		ToolProperty("a", "string", "a"),
	).DisallowAdditionalProperties().Build()
	if got := schemaMap(t, def)["additionalProperties"]; got != false {
		t.Fatalf("additionalProperties = %v, want false", got)
	}

	open := DefineSchema("t", "d", ToolProperty("a", "string", "a")).Build()
	if _, present := schemaMap(t, open)["additionalProperties"]; present {
		t.Fatal("default schema should omit additionalProperties")
	}
}
