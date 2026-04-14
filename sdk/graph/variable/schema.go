package variable

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Schema defines the input/output variable schema for an App or node.
type Schema struct {
	Variables []Variable `json:"variables" yaml:"variables"`
}

// NewSchema creates a new schema from variable definitions.
func NewSchema(vars ...Variable) *Schema {
	return &Schema{Variables: vars}
}

// Validate validates a set of values against the schema.
// Returns a validation error aggregating all field-level messages.
// Returns nil if no errors.
func (s *Schema) Validate(values map[string]any) error {
	if s == nil {
		return nil
	}
	var msgs []string
	for _, v := range s.Variables {
		val, exists := values[v.Name]
		if !exists {
			if v.Required {
				msgs = append(msgs, fmt.Sprintf("missing required variable %q", v.Name))
			}
			continue
		}
		if err := v.Validate(val); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		return errdefs.Validationf("schema validation failed: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// ApplyDefaults fills in default values for missing variables.
// Returns a new map; the original is not modified.
func (s *Schema) ApplyDefaults(values map[string]any) map[string]any {
	if s == nil {
		return values
	}
	result := make(map[string]any, len(values))
	for k, v := range values {
		result[k] = v
	}
	for _, v := range s.Variables {
		if _, exists := result[v.Name]; !exists {
			if def := v.ApplyDefault(nil); def != nil {
				result[v.Name] = def
			}
		}
	}
	return result
}

// Get returns a variable by name.
func (s *Schema) Get(name string) (Variable, bool) {
	for _, v := range s.Variables {
		if v.Name == name {
			return v, true
		}
	}
	return Variable{}, false
}

// Names returns all variable names.
func (s *Schema) Names() []string {
	names := make([]string, len(s.Variables))
	for i, v := range s.Variables {
		names[i] = v.Name
	}
	return names
}

// RequiredNames returns the names of all required variables.
func (s *Schema) RequiredNames() []string {
	var names []string
	for _, v := range s.Variables {
		if v.Required {
			names = append(names, v.Name)
		}
	}
	return names
}

// ToJSONSchema converts the schema to a JSON Schema map (for LLM tool definitions).
func (s *Schema) ToJSONSchema() map[string]any {
	props := make(map[string]any, len(s.Variables))
	var required []string

	for _, v := range s.Variables {
		prop := map[string]any{
			"type": string(v.Type),
		}
		if v.Description != "" {
			prop["description"] = v.Description
		}
		if len(v.Enum) > 0 {
			prop["enum"] = v.Enum
		}
		if v.MaxLength > 0 {
			prop["maxLength"] = v.MaxLength
		}
		if v.Type == TypeArray && v.ItemType != "" {
			prop["items"] = map[string]any{"type": string(v.ItemType)}
		}
		if v.Type == TypeObject && len(v.Properties) > 0 {
			subSchema := NewSchema(v.Properties...)
			prop["properties"] = subSchema.ToJSONSchema()["properties"]
		}
		props[v.Name] = prop
		if v.Required {
			required = append(required, v.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
