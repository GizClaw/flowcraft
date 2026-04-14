package variable

import (
	"strings"
	"testing"
)

func TestVariable_Validate(t *testing.T) {
	tests := []struct {
		name    string
		v       Variable
		value   any
		wantErr bool
	}{
		{"string ok", Variable{Name: "x", Type: TypeString}, "hello", false},
		{"string type error", Variable{Name: "x", Type: TypeString}, 42, true},
		{"number ok int", Variable{Name: "x", Type: TypeNumber}, 42, false},
		{"number ok float64", Variable{Name: "x", Type: TypeNumber}, 3.14, false},
		{"number type error", Variable{Name: "x", Type: TypeNumber}, "abc", true},
		{"integer ok", Variable{Name: "x", Type: TypeInteger}, 42, false},
		{"integer rejects float", Variable{Name: "x", Type: TypeInteger}, 3.14, true},
		{"boolean ok", Variable{Name: "x", Type: TypeBoolean}, true, false},
		{"boolean type error", Variable{Name: "x", Type: TypeBoolean}, "true", true},
		{"array ok", Variable{Name: "x", Type: TypeArray}, []any{1, 2}, false},
		{"array ok []int", Variable{Name: "x", Type: TypeArray}, []int{1, 2}, false},
		{"array ok []int64", Variable{Name: "x", Type: TypeArray}, []int64{1, 2}, false},
		{"array ok []float64", Variable{Name: "x", Type: TypeArray}, []float64{1, 2}, false},
		{"array ok []bool", Variable{Name: "x", Type: TypeArray}, []bool{true, false}, false},
		{"object ok", Variable{Name: "x", Type: TypeObject}, map[string]any{"k": "v"}, false},
		{"file string ok", Variable{Name: "x", Type: TypeFile}, "/path/to/file", false},
		{"any ok", Variable{Name: "x", Type: TypeAny}, "anything", false},
		{"required nil", Variable{Name: "x", Type: TypeString, Required: true}, nil, true},
		{"optional nil", Variable{Name: "x", Type: TypeString}, nil, false},
		{"maxlength ok", Variable{Name: "x", Type: TypeString, MaxLength: 5}, "hello", false},
		{"maxlength exceeded", Variable{Name: "x", Type: TypeString, MaxLength: 3}, "hello", true},
		{"enum ok", Variable{Name: "x", Type: TypeString, Enum: []any{"a", "b"}}, "a", false},
		{"enum rejected", Variable{Name: "x", Type: TypeString, Enum: []any{"a", "b"}}, "c", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.v.Validate(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSchema_Validate(t *testing.T) {
	s := NewSchema(
		Variable{Name: "topic", Type: TypeString, Required: true},
		Variable{Name: "depth", Type: TypeInteger, DefaultValue: 3},
	)

	err := s.Validate(map[string]any{"topic": "AI"})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	err = s.Validate(map[string]any{})
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
}

func TestSchema_ApplyDefaults(t *testing.T) {
	s := NewSchema(
		Variable{Name: "topic", Type: TypeString, Required: true},
		Variable{Name: "depth", Type: TypeInteger, DefaultValue: 3},
	)
	result := s.ApplyDefaults(map[string]any{"topic": "AI"})
	if result["depth"] != 3 {
		t.Fatalf("expected depth=3, got %v", result["depth"])
	}
}

func TestSchema_ToJSONSchema(t *testing.T) {
	s := NewSchema(
		Variable{Name: "name", Type: TypeString, Required: true, Description: "User name"},
		Variable{Name: "age", Type: TypeInteger},
	)
	js := s.ToJSONSchema()
	if js["type"] != "object" {
		t.Fatal("expected type object")
	}
	props, ok := js["properties"].(map[string]any)
	if !ok || len(props) != 2 {
		t.Fatal("expected 2 properties")
	}
	req, ok := js["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "name" {
		t.Fatalf("expected required=[name], got %v", req)
	}
}

func TestResolver_Resolve(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"topic": "机器学习", "depth": 3})
	r.AddScope("env", map[string]any{"API_KEY": "sk-123"})

	result := r.Resolve("你是一个${input.topic}专家，深度${input.depth}")
	if !strings.Contains(result, "机器学习") {
		t.Fatalf("expected resolved string, got %q", result)
	}
	if !strings.Contains(result, "3") {
		t.Fatalf("expected resolved depth, got %q", result)
	}
}

func TestResolver_ResolveMap(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"topic": "AI"})

	result, err := r.ResolveMap(map[string]any{
		"prompt":  "研究${input.topic}",
		"timeout": 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["prompt"] != "研究AI" {
		t.Fatalf("expected '研究AI', got %q", result["prompt"])
	}
	if result["timeout"] != 30 {
		t.Fatalf("expected 30, got %v", result["timeout"])
	}
}

func TestResolver_UnresolvedKept(t *testing.T) {
	r := NewResolver()
	result := r.Resolve("${unknown.var} stays")
	if result != "${unknown.var} stays" {
		t.Fatalf("expected unresolved reference kept, got %q", result)
	}
}

func TestExtractRefs(t *testing.T) {
	refs := ExtractRefs("Hello ${input.topic} and ${env.KEY}")
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
}
