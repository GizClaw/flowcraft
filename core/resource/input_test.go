package resource

import (
	"testing"
)

func TestInputDepsMany(t *testing.T) {
	in := Input{Deps: map[string]any{
		"provider.openai": "openai",
		"provider.qwen":   "qwen",
		"provider":        "single",
		"other":           "x",
	}}
	got := in.DepsMany("provider")
	if len(got) != 3 {
		t.Fatalf("DepsMany = %v, want 3 values", got)
	}
	if got[0] != "single" || got[1] != "openai" || got[2] != "qwen" {
		t.Fatalf("DepsMany order = %v, want [single openai qwen]", got)
	}
}

func TestInputDepsManyEmpty(t *testing.T) {
	in := Input{Deps: map[string]any{"other": "x"}}
	if got := in.DepsMany("provider"); len(got) != 0 {
		t.Fatalf("DepsMany = %v, want empty", got)
	}
}

func TestInputDep(t *testing.T) {
	in := Input{Deps: map[string]any{"workspace": "fs"}}
	v, ok := in.Dep("workspace")
	if !ok || v != "fs" {
		t.Fatalf("Dep = (%v, %v)", v, ok)
	}
	if _, ok := in.Dep("missing"); ok {
		t.Fatal("Dep found missing key")
	}
}
