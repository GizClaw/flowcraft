// Package variable provides a typed variable system for the graph engine.
// It supports variable schemas, reference resolution, validation, and
// default value application.
package variable

import (
	"fmt"
	"math"
	"unicode/utf8"
)

// Type represents a variable data type.
type Type string

const (
	TypeString  Type = "string"
	TypeNumber  Type = "number"
	TypeInteger Type = "integer"
	TypeBoolean Type = "boolean"
	TypeArray   Type = "array"
	TypeObject  Type = "object"
	TypeFile    Type = "file"
	TypeAny     Type = "any"
)

// Variable represents a typed variable with metadata.
type Variable struct {
	Name         string     `json:"name" yaml:"name"`
	Type         Type       `json:"type" yaml:"type"`
	Description  string     `json:"description,omitempty" yaml:"description,omitempty"`
	Required     bool       `json:"required,omitempty" yaml:"required,omitempty"`
	DefaultValue any        `json:"default,omitempty" yaml:"default,omitempty"`
	MaxLength    int        `json:"max_length,omitempty" yaml:"max_length,omitempty"`
	Enum         []any      `json:"enum,omitempty" yaml:"enum,omitempty"`
	ItemType     Type       `json:"item_type,omitempty" yaml:"item_type,omitempty"`
	Properties   []Variable `json:"properties,omitempty" yaml:"properties,omitempty"`
}

// Validate checks if a value matches the variable's type constraints.
func (v *Variable) Validate(value any) error {
	if value == nil {
		if v.Required {
			return fmt.Errorf("variable %q is required", v.Name)
		}
		return nil
	}

	if err := v.validateType(value); err != nil {
		return err
	}

	if len(v.Enum) > 0 {
		found := false
		for _, e := range v.Enum {
			if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", value) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("variable %q value %v not in enum %v", v.Name, value, v.Enum)
		}
	}

	if v.MaxLength > 0 && v.Type == TypeString {
		if str, ok := value.(string); ok && utf8.RuneCountInString(str) > v.MaxLength {
			return fmt.Errorf("variable %q exceeds max length %d", v.Name, v.MaxLength)
		}
	}

	return nil
}

func (v *Variable) validateType(value any) error {
	switch v.Type {
	case TypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("variable %q: expected string, got %T", v.Name, value)
		}
	case TypeNumber:
		switch value.(type) {
		case float64, float32, int, int64, int32:
		default:
			return fmt.Errorf("variable %q: expected number, got %T", v.Name, value)
		}
	case TypeInteger:
		switch val := value.(type) {
		case int, int64, int32:
		case float64:
			if val != math.Trunc(val) || math.IsInf(val, 0) || math.IsNaN(val) {
				return fmt.Errorf("variable %q: expected integer, got non-integer float64", v.Name)
			}
		default:
			return fmt.Errorf("variable %q: expected integer, got %T", v.Name, value)
		}
	case TypeBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("variable %q: expected boolean, got %T", v.Name, value)
		}
	case TypeArray:
		switch value.(type) {
		case []any, []string, []map[string]any, []int, []int64, []float64, []bool:
		default:
			return fmt.Errorf("variable %q: expected array, got %T", v.Name, value)
		}
	case TypeObject:
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("variable %q: expected object, got %T", v.Name, value)
		}
	case TypeFile:
		switch value.(type) {
		case string, map[string]any:
		default:
			return fmt.Errorf("variable %q: expected file path or metadata, got %T", v.Name, value)
		}
	case TypeAny:
		// Any type accepted.
	default:
		return fmt.Errorf("variable %q: unknown type %q", v.Name, v.Type)
	}
	return nil
}

// ApplyDefault returns the default value if value is nil.
func (v *Variable) ApplyDefault(value any) any {
	if value == nil && v.DefaultValue != nil {
		return v.DefaultValue
	}
	return value
}
