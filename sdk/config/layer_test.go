package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadLayers_DeepMergeAndOrigins(t *testing.T) {
	loader := NewLoader()
	layered, err := loader.LoadLayers(context.Background(), []Layer{
		{
			Name:   "defaults",
			Source: LiteralSource("a: 1\nnested:\n  x: 1\n  list: [1, 2]\n"),
		},
		{
			Name:   "overrides",
			Source: LiteralSource("a: 2\nnested:\n  second: 2\n  list: [3]\n"),
		},
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(layered.Data, &got); err != nil {
		t.Fatalf("merged data is not an object: %v\n%s", err, layered.Data)
	}
	want := map[string]any{
		"a": float64(2),
		"nested": map[string]any{
			"x":      float64(1),
			"second": float64(2),
			"list":   []any{float64(3)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %#v, want %#v", got, want)
	}
	wantOrigins := map[string]string{
		"a":             "overrides",
		"nested.x":      "defaults",
		"nested.second": "overrides",
		"nested.list":   "overrides",
	}
	if !reflect.DeepEqual(layered.Origins, wantOrigins) {
		t.Errorf("origins = %#v, want %#v", layered.Origins, wantOrigins)
	}
}

func TestLoadLayers_NullDeletesKey(t *testing.T) {
	loader := NewLoader()
	layered, err := loader.LoadLayers(context.Background(), []Layer{
		{Name: "base", Source: LiteralSource("a: 1\nb: 2\n")},
		{Name: "override", Source: LiteralSource("a: null\n")},
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(layered.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["a"]; ok {
		t.Errorf("merged still contains deleted key a: %#v", got)
	}
	if got["b"] != float64(2) {
		t.Errorf("merged b = %v, want 2", got["b"])
	}
}

func TestLoadLayers_FileLayersResolveAgainstBaseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "defaults.yaml"), []byte("a: 1\n"), 0o600); err != nil {
		t.Fatalf("write defaults: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.yaml"), []byte("a: 3\nb: 4\n"), 0o600); err != nil {
		t.Fatalf("write host: %v", err)
	}
	loader := NewLoader(WithBaseDir(dir))
	layered, err := loader.LoadLayers(context.Background(), []Layer{
		{Name: "defaults", Source: FileSource("./defaults.yaml")},
		{Name: "host", Source: FileSource("./host.yaml")},
	})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(layered.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["a"] != float64(3) || got["b"] != float64(4) {
		t.Errorf("merged = %#v, want a=3 b=4", got)
	}
	if layered.Origins["a"] != "host" {
		t.Errorf("origin of a = %q, want host", layered.Origins["a"])
	}
}

func TestLoadLayers_RejectsBadInput(t *testing.T) {
	loader := NewLoader()
	if _, err := loader.LoadLayers(context.Background(), nil); err == nil {
		t.Error("empty layers accepted, want error")
	}
	if _, err := loader.LoadLayers(context.Background(), []Layer{
		{Name: " ", Source: LiteralSource("a: 1")},
	}); err == nil {
		t.Error("blank layer name accepted, want error")
	}
	if _, err := loader.LoadLayers(context.Background(), []Layer{
		{Name: "list", Source: LiteralSource("- 1\n- 2")},
	}); err == nil {
		t.Error("non-object layer accepted, want error")
	}
}
