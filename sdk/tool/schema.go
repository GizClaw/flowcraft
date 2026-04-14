package tool

import "github.com/GizClaw/flowcraft/sdk/model"

// PropertyDef describes a single JSON Schema property.
type PropertyDef struct {
	name   string
	schema map[string]any
}

// Property creates a simple typed property definition.
func Property(name, typ, description string) PropertyDef {
	return PropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
		},
	}
}

// ArrayProperty creates an array property with item schema.
func ArrayProperty(name, description string, items map[string]any) PropertyDef {
	return PropertyDef{
		name: name,
		schema: map[string]any{
			"type":        "array",
			"description": description,
			"items":       items,
		},
	}
}

// ObjectProperty creates an object property with nested properties schema.
func ObjectProperty(name, description string, properties map[string]any) PropertyDef {
	schema := map[string]any{
		"type":        "object",
		"description": description,
	}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	return PropertyDef{name: name, schema: schema}
}

// PropertyWithDefault creates a typed property with a default value.
func PropertyWithDefault(name, typ, description string, defaultVal any) PropertyDef {
	return PropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
			"default":     defaultVal,
		},
	}
}

// EnumProperty creates a property restricted to a set of string values.
func EnumProperty(name, typ, description string, values ...string) PropertyDef {
	enums := make([]any, len(values))
	for i, v := range values {
		enums[i] = v
	}
	return PropertyDef{
		name: name,
		schema: map[string]any{
			"type":        typ,
			"description": description,
			"enum":        enums,
		},
	}
}

// SchemaBuilder constructs a ToolDefinition using a fluent API.
type SchemaBuilder struct {
	name        string
	description string
	properties  map[string]any
	required    []string
}

// DefineSchema starts building a ToolDefinition with the given properties.
func DefineSchema(name, description string, props ...PropertyDef) *SchemaBuilder {
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

// Build returns the final ToolDefinition.
func (b *SchemaBuilder) Build() model.ToolDefinition {
	schema := map[string]any{
		"type":       "object",
		"properties": b.properties,
	}
	if len(b.required) > 0 {
		schema["required"] = b.required
	}
	return model.ToolDefinition{
		Name:        b.name,
		Description: b.description,
		InputSchema: schema,
	}
}
