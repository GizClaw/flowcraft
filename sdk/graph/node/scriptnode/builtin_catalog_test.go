package scriptnode

import (
	"reflect"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
)

func TestBuiltinCatalog_CoversEmbeddedScripts(t *testing.T) {
	seen := map[string]bool{}
	var catalogTypes []string
	for _, spec := range builtinCatalog {
		if seen[spec.Type()] {
			t.Fatalf("duplicate builtin catalog entry for %q", spec.Type())
		}
		seen[spec.Type()] = true
		catalogTypes = append(catalogTypes, spec.Type())
		if _, err := spec.Source(); err != nil {
			t.Fatalf("catalog source for %q: %v", spec.Type(), err)
		}
	}

	embeddedTypes := scripts.BuiltinTypes()
	for _, nodeType := range embeddedTypes {
		if !seen[nodeType] {
			t.Fatalf("embedded script %q is missing from builtin catalog", nodeType)
		}
	}
	if len(catalogTypes) != len(embeddedTypes) {
		sort.Strings(catalogTypes)
		sort.Strings(embeddedTypes)
		t.Fatalf("builtin catalog type count = %d, embedded script count = %d\ncatalog: %v\nembedded: %v",
			len(catalogTypes), len(embeddedTypes), catalogTypes, embeddedTypes)
	}
}

func TestBuiltinCatalog_PortsAreExplicit(t *testing.T) {
	genericInput, genericOutput := genericScriptPorts()
	for _, spec := range builtinCatalog {
		input, output := spec.Ports()
		if len(input)+len(output) == 0 {
			t.Fatalf("%s has no declared ports", spec.Type())
		}
		if reflect.DeepEqual(input, genericInput) && reflect.DeepEqual(output, genericOutput) {
			t.Fatalf("%s uses generic fallback ports", spec.Type())
		}
	}
}

func TestBuiltinCatalog_DefaultBridgeRules(t *testing.T) {
	for _, spec := range builtinCatalog {
		switch spec.Type() {
		case "context", "gate":
			if spec.needsCommandRunner(nil) {
				t.Fatalf("%s should not need CommandRunner without commands", spec.Type())
			}
			if spec.needsCommandRunner(map[string]any{"commands": []any{}}) {
				t.Fatalf("%s should not need CommandRunner with empty commands", spec.Type())
			}
			if !spec.needsCommandRunner(map[string]any{"commands": []any{"echo hi"}}) {
				t.Fatalf("%s should need CommandRunner with non-empty commands", spec.Type())
			}
		default:
			if spec.needsCommandRunner(map[string]any{"commands": []any{"echo hi"}}) {
				t.Fatalf("%s should not need CommandRunner", spec.Type())
			}
		}
	}
}
