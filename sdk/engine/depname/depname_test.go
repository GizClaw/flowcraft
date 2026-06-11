package depname_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
)

// TestConstants_Stable pins the conventional spellings. Renaming any
// of these is a breaking change for capability declarations and host
// wiring written against the previous string — the test exists so
// such a rename is an explicit code-review event, not an accidental
// rg-and-replace casualty.
func TestConstants_Stable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"LLMClient", depname.LLMClient, "llm.client"},
		{"LLMResolver", depname.LLMResolver, "llm.resolver"},
		{"ToolRegistry", depname.ToolRegistry, "tool.registry"},
		{"ToolAllowedNames", depname.ToolAllowedNames, "tool.allowed_names"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (renaming this is a wire-format breaking change)",
				tc.name, tc.got, tc.want)
		}
	}
}

// TestConstants_NamingConvention enforces the documented
// <package>.<noun> shape. The convention is documented in the
// package godoc; this test guards against drift when new constants
// are added.
func TestConstants_NamingConvention(t *testing.T) {
	all := []string{
		depname.LLMClient,
		depname.LLMResolver,
		depname.ToolRegistry,
		depname.ToolAllowedNames,
	}
	for _, n := range all {
		if n == "" {
			t.Errorf("empty depname constant")
			continue
		}
		if !strings.Contains(n, ".") {
			t.Errorf("%q missing '.': depname constants must follow <package>.<noun>", n)
		}
		if strings.ToLower(n) != n {
			t.Errorf("%q is not lower-case: depname constants are lower-snake-joined-by-dot", n)
		}
	}
}

// TestConstants_NoDuplicates prevents two constants accidentally
// resolving to the same string (which would silently make one of
// them unreachable through the container).
func TestConstants_NoDuplicates(t *testing.T) {
	all := map[string]string{
		"LLMClient":        depname.LLMClient,
		"LLMResolver":      depname.LLMResolver,
		"ToolRegistry":     depname.ToolRegistry,
		"ToolAllowedNames": depname.ToolAllowedNames,
	}
	seen := make(map[string]string, len(all))
	for sym, val := range all {
		if other, dup := seen[val]; dup {
			t.Errorf("duplicate value %q on %s and %s", val, sym, other)
		}
		seen[val] = sym
	}
}

// TestUsable_WithDependencies sanity-checks the round-trip with
// engine.Dependencies + engine.GetDep so the docstring example is
// guaranteed to compile and behave as advertised.
func TestUsable_WithDependencies(t *testing.T) {
	type fakeRegistry struct{ id string }

	deps := engine.NewDependencies()
	want := &fakeRegistry{id: "r1"}
	deps.Set(depname.ToolRegistry, want)

	got, err := engine.GetDep[*fakeRegistry](deps, depname.ToolRegistry)
	if err != nil {
		t.Fatalf("GetDep returned %v", err)
	}
	if got != want {
		t.Fatalf("GetDep returned %p, want %p", got, want)
	}
}
