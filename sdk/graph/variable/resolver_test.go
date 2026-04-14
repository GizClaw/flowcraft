package variable

import (
	"strings"
	"testing"
)

func TestResolver_Clone_Independent(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"x": 1})

	cloned := r.Clone()
	cloned.AddScope("input", map[string]any{"x": 99, "y": 2})

	orig := r.Resolve("${input.x}")
	if orig != "1" {
		t.Fatalf("original resolver should be unaffected, got %q", orig)
	}

	cl := cloned.Resolve("${input.x}")
	if cl != "99" {
		t.Fatalf("cloned resolver should see new value, got %q", cl)
	}

	oy := r.Resolve("${input.y}")
	if oy != "${input.y}" {
		t.Fatalf("original should not have 'y', got %q", oy)
	}
}

func TestResolver_Clone_EmptyResolver(t *testing.T) {
	r := NewResolver()
	cloned := r.Clone()
	cloned.AddScope("test", map[string]any{"a": "b"})

	result := cloned.Resolve("${test.a}")
	if result != "b" {
		t.Fatalf("cloned empty resolver should work, got %q", result)
	}

	result = r.Resolve("${test.a}")
	if result != "${test.a}" {
		t.Fatalf("original should not be affected, got %q", result)
	}
}

func TestResolver_ResolveMap_NestedValues(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"context": map[string]any{
			"user": map[string]any{"name": "Alice"},
		},
	})

	result, err := r.ResolveMap(map[string]any{
		"greeting": "Hello ${board.context.user.name}",
		"count":    42,
		"flag":     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["greeting"] != "Hello Alice" {
		t.Fatalf("greeting = %q", result["greeting"])
	}
	if result["count"] != 42 {
		t.Fatal("non-string values should pass through")
	}
	if result["flag"] != true {
		t.Fatal("boolean should pass through")
	}
}

func TestResolver_ResolveMap_MultipleRefsInOneString(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"name": "Bob", "role": "admin"})

	result, err := r.ResolveMap(map[string]any{
		"prompt": "User: ${input.name}, Role: ${input.role}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["prompt"] != "User: Bob, Role: admin" {
		t.Fatalf("prompt = %q", result["prompt"])
	}
}

func TestResolver_Resolve_NonStringValue_JSON(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"items": []string{"a", "b", "c"},
	})

	result := r.Resolve("items: ${board.items}")
	if !strings.Contains(result, `["a","b","c"]`) {
		t.Fatalf("expected JSON array, got %q", result)
	}
}

func TestResolver_Resolve_WholeStringReplacement(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"topic": "AI"})

	result := r.Resolve("${input.topic}")
	if result != "AI" {
		t.Fatalf("expected 'AI', got %q", result)
	}
}

func TestResolver_UnknownScope_LeavesReference(t *testing.T) {
	r := NewResolver()
	result := r.Resolve("${nonexistent.var}")
	if result != "${nonexistent.var}" {
		t.Fatalf("expected reference unchanged, got %q", result)
	}
}

func TestResolver_UnknownKey_LeavesReference(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"x": 1})

	result := r.Resolve("val=${input.missing}")
	if result != "val=${input.missing}" {
		t.Fatalf("expected reference preserved, got %q", result)
	}
}

func TestResolver_ResolveMap_EmptyMap(t *testing.T) {
	r := NewResolver()
	result, err := r.ResolveMap(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %v", result)
	}
}

func TestResolver_Resolve_NoRefs(t *testing.T) {
	r := NewResolver()
	result := r.Resolve("plain text with no refs")
	if result != "plain text with no refs" {
		t.Fatalf("got %q", result)
	}
}

func TestResolver_Clone_DeepCopy(t *testing.T) {
	shared := map[string]any{"key": "original"}
	r := NewResolver()
	r.AddScope("data", shared)

	cloned := r.Clone()

	// Mutate the cloned resolver's inner map directly
	cloned.scopes["data"]["key"] = "mutated"
	cloned.scopes["data"]["extra"] = "new"

	origVal := r.Resolve("${data.key}")
	if origVal != "original" {
		t.Fatalf("deep clone failed: original resolver was mutated, got %q", origVal)
	}
	origExtra := r.Resolve("${data.extra}")
	if origExtra != "${data.extra}" {
		t.Fatalf("deep clone failed: original resolver gained extra key, got %q", origExtra)
	}
}

func TestResolver_Clone_DeepCopy_NestedMap(t *testing.T) {
	r := NewResolver()
	r.AddScope("data", map[string]any{
		"nested": map[string]any{
			"inner": "original",
		},
		"list": []any{"a", "b"},
	})

	cloned := r.Clone()

	cloned.scopes["data"]["nested"].(map[string]any)["inner"] = "mutated"
	cloned.scopes["data"]["list"].([]any)[0] = "X"

	origVal := r.Resolve("${data.nested.inner}")
	if origVal != "original" {
		t.Fatalf("nested map deep clone failed: original was mutated, got %q", origVal)
	}

	origList := r.scopes["data"]["list"].([]any)
	if origList[0] != "a" {
		t.Fatalf("slice deep clone failed: original was mutated, got %v", origList[0])
	}
}

func TestResolver_Clone_DeepCopy_DeeplyNested(t *testing.T) {
	r := NewResolver()
	r.AddScope("cfg", map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"value": "deep",
			},
		},
	})

	cloned := r.Clone()
	cloned.scopes["cfg"]["level1"].(map[string]any)["level2"].(map[string]any)["value"] = "changed"

	origVal := r.Resolve("${cfg.level1.level2.value}")
	if origVal != "deep" {
		t.Fatalf("deeply nested clone failed: original was mutated, got %q", origVal)
	}
}

func TestResolver_Clone_DeepCopy_SliceOfMaps(t *testing.T) {
	r := NewResolver()
	r.AddScope("items", map[string]any{
		"records": []any{
			map[string]any{"name": "alice"},
			map[string]any{"name": "bob"},
		},
	})

	cloned := r.Clone()
	cloned.scopes["items"]["records"].([]any)[0].(map[string]any)["name"] = "changed"

	origRecords := r.scopes["items"]["records"].([]any)
	origName := origRecords[0].(map[string]any)["name"]
	if origName != "alice" {
		t.Fatalf("slice-of-maps deep clone failed: original was mutated, got %v", origName)
	}
}

func TestResolver_AddScope_Overwrites(t *testing.T) {
	r := NewResolver()
	r.AddScope("input", map[string]any{"x": "old"})
	r.AddScope("input", map[string]any{"x": "new"})

	result := r.Resolve("${input.x}")
	if result != "new" {
		t.Fatalf("expected 'new' after overwrite, got %q", result)
	}
}

func TestResolver_ResolveMap_TypedFloat(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"temperature": 0.3,
	})

	result, err := r.ResolveMap(map[string]any{
		"temperature": "${board.temperature}",
	})
	if err != nil {
		t.Fatal(err)
	}
	temp, ok := result["temperature"].(float64)
	if !ok {
		t.Fatalf("expected float64, got %T", result["temperature"])
	}
	if temp != 0.3 {
		t.Fatalf("expected 0.3, got %f", temp)
	}
}

func TestResolver_ResolveMap_TypedBool(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"json_mode": true,
	})

	result, err := r.ResolveMap(map[string]any{
		"json_mode": "${board.json_mode}",
	})
	if err != nil {
		t.Fatal(err)
	}
	val, ok := result["json_mode"].(bool)
	if !ok {
		t.Fatalf("expected bool, got %T", result["json_mode"])
	}
	if !val {
		t.Fatal("expected true")
	}
}

func TestResolver_ResolveMap_TypedInt(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"max_tokens": 2048,
	})

	result, err := r.ResolveMap(map[string]any{
		"max_tokens": "${board.max_tokens}",
	})
	if err != nil {
		t.Fatal(err)
	}
	val, ok := result["max_tokens"].(int)
	if !ok {
		t.Fatalf("expected int, got %T", result["max_tokens"])
	}
	if val != 2048 {
		t.Fatalf("expected 2048, got %d", val)
	}
}

func TestResolver_ResolveMap_MixedRef_StaysString(t *testing.T) {
	r := NewResolver()
	r.AddScope("board", map[string]any{
		"temperature": 0.5,
	})

	result, err := r.ResolveMap(map[string]any{
		"label": "temp is ${board.temperature}",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := result["label"].(string)
	if !ok {
		t.Fatalf("expected string, got %T", result["label"])
	}
	if s != "temp is 0.5" {
		t.Fatalf("got %q", s)
	}
}

func TestResolver_ResolveMap_TypedUnresolved_StaysString(t *testing.T) {
	r := NewResolver()

	result, err := r.ResolveMap(map[string]any{
		"temperature": "${board.missing}",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := result["temperature"].(string)
	if !ok {
		t.Fatalf("expected string for unresolved ref, got %T", result["temperature"])
	}
	if s != "${board.missing}" {
		t.Fatalf("got %q", s)
	}
}
