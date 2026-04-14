package scripts

import (
	"sort"
	"testing"
)

func TestBuiltinTypes_MatchesEmbeddedFiles(t *testing.T) {
	entries, err := scriptsFS.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var expected []string
	for _, e := range entries {
		if !e.IsDir() && len(e.Name()) > 3 && e.Name()[len(e.Name())-3:] == ".js" {
			expected = append(expected, e.Name()[:len(e.Name())-3])
		}
	}

	got := BuiltinTypes()

	sort.Strings(expected)
	sort.Strings(got)

	if len(got) != len(expected) {
		t.Fatalf("BuiltinTypes() returned %d types, want %d\ngot:  %v\nwant: %v", len(got), len(expected), got, expected)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("mismatch at %d: got %q, want %q", i, got[i], expected[i])
		}
	}
}

func TestBuiltinTypes_NonEmpty(t *testing.T) {
	types := BuiltinTypes()
	if len(types) == 0 {
		t.Fatal("BuiltinTypes should not be empty")
	}
}

func TestBuiltinTypes_ReturnsCopy(t *testing.T) {
	a := BuiltinTypes()
	b := BuiltinTypes()
	a[0] = "modified"
	if b[0] == "modified" {
		t.Fatal("BuiltinTypes should return a copy, not a reference")
	}
}

func TestIsBuiltin(t *testing.T) {
	if !IsBuiltin("router") {
		t.Fatal("router should be builtin")
	}
	if !IsBuiltin("ifelse") {
		t.Fatal("ifelse should be builtin")
	}
	if IsBuiltin("nonexistent_type") {
		t.Fatal("nonexistent_type should not be builtin")
	}
}

func TestGet_Known(t *testing.T) {
	src, err := Get("router")
	if err != nil {
		t.Fatalf("Get(router): %v", err)
	}
	if src == "" {
		t.Fatal("router source should not be empty")
	}
}

func TestGet_Unknown(t *testing.T) {
	_, err := Get("nonexistent_type")
	if err == nil {
		t.Fatal("Get should return error for unknown type")
	}
}

func TestMustGet_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustGet should panic for unknown type")
		}
	}()
	MustGet("nonexistent_type")
}
