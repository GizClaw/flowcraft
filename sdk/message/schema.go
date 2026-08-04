package message

import "encoding/json"

// ToolPropertyDef describes a single JSON Schema property.
type ToolPropertyDef struct {
	name   string
	schema map[string]any
}

// ToolProperty creates a simple typed property definition.
func ToolProperty(name, typ, description string) ToolPropertyDef {
	return ToolPropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
		},
	}
}

// Items is the schema for the elements of a ToolArrayProperty: the
// common "array of plain-typed values" case without a raw map.
func Items(typ string) map[string]any {
	return map[string]any{"type": typ}
}

// ToolArrayProperty creates an array property with item schema.
func ToolArrayProperty(name, description string, items map[string]any) ToolPropertyDef {
	return ToolPropertyDef{
		name: name,
		schema: map[string]any{
			"type":        "array",
			"description": description,
			"items":       items,
		},
	}
}

// ToolObjectProperty creates an object property with nested properties schema.
func ToolObjectProperty(name, description string, properties map[string]any) ToolPropertyDef {
	schema := map[string]any{
		"type":        "object",
		"description": description,
	}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	return ToolPropertyDef{name: name, schema: schema}
}

// ToolStringMapProperty creates an object property whose values must be strings.
func ToolStringMapProperty(name, description string) ToolPropertyDef {
	return ToolPropertyDef{
		name: name,
		schema: map[string]any{
			"type":                 "object",
			"description":          description,
			"additionalProperties": map[string]any{"type": "string"},
		},
	}
}

// ToolPropertyWithDefault creates a typed property with a default value.
func ToolPropertyWithDefault(name, typ, description string, defaultVal any) ToolPropertyDef {
	return ToolPropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
			"default":     defaultVal,
		},
	}
}

// ToolEnumProperty creates a property restricted to a set of string values.
func ToolEnumProperty(name, typ, description string, values ...string) ToolPropertyDef {
	enums := make([]any, len(values))
	for i, v := range values {
		enums[i] = v
	}
	return ToolPropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
			"enum":        enums,
		},
	}
}

// SchemaBuilder constructs a Definition using a fluent API.
type SchemaBuilder struct {
	name        string
	description string
	properties  map[string]any
	required    []string
	closed      bool
}

// DefineSchema starts building a Definition with the given properties.
func DefineSchema(name, description string, props ...ToolPropertyDef) *SchemaBuilder {
	properties := make(map[string]any, len(props))
	for _, p := range props {
		properties[p.name] = p.schema
	}
	return &SchemaBuilder{
		name:        name,
		description: description,
		properties:  properties,
	}
}

// DisallowAdditionalProperties marks the object schema as closed:
// providers that honor additionalProperties will reject arguments
// outside the declared properties.
func (b *SchemaBuilder) DisallowAdditionalProperties() *SchemaBuilder {
	b.closed = true
	return b
}

// Required marks the given property names as required in the JSON Schema.
// Duplicate names are silently ignored.
func (b *SchemaBuilder) Required(names ...string) *SchemaBuilder {
	seen := make(map[string]bool, len(b.required))
	for _, n := range b.required {
		seen[n] = true
	}
	for _, n := range names {
		if !seen[n] {
			b.required = append(b.required, n)
			seen[n] = true
		}
	}
	return b
}

// Build returns the final Definition.
func (b *SchemaBuilder) Build() Definition {
	schema := map[string]any{
		"type":       "object",
		"properties": b.properties,
	}
	if len(b.required) > 0 {
		schema["required"] = b.required
	}
	if b.closed {
		schema["additionalProperties"] = false
	}
	raw, _ := json.Marshal(schema)
	return Definition{
		Name:        b.name,
		Description: b.description,
		InputSchema: raw,
	}
}
