package scriptnode

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
	nodepkg "github.com/GizClaw/flowcraft/sdk/graph/node"
)

func TestScriptNodeNew_UsesBuiltinPorts(t *testing.T) {
	for nodeType, want := range expectedBuiltinPorts() {
		t.Run(nodeType, func(t *testing.T) {
			n := New(nodeType, nodeType, "", nil, nil)
			if got := n.InputPorts(); !reflect.DeepEqual(got, want.input) {
				t.Fatalf("input ports = %v, want %v", got, want.input)
			}
			if got := n.OutputPorts(); !reflect.DeepEqual(got, want.output) {
				t.Fatalf("output ports = %v, want %v", got, want.output)
			}
		})
	}
}

func TestBuiltinPortsMirroredToGlobalRegistry(t *testing.T) {
	for nodeType, want := range expectedBuiltinPorts() {
		t.Run(nodeType, func(t *testing.T) {
			input, output := nodepkg.PortsForType(nodeType)
			if !reflect.DeepEqual(input, want.input) || !reflect.DeepEqual(output, want.output) {
				t.Fatalf("global ports = (%v, %v), want (%v, %v)", input, output, want.input, want.output)
			}
		})
	}
}

func TestScriptNodeNew_UsesRegisteredPortsForNonBuiltinTypes(t *testing.T) {
	nodepkg.RegisterPorts("__custom_scriptnode_ports__", []graph.Port{
		{Name: "custom_in", Type: graph.PortTypeString},
	}, []graph.Port{
		{Name: "custom_out", Type: graph.PortTypeBool},
	})

	n := New("custom", "__custom_scriptnode_ports__", "", nil, nil)
	if got := n.InputPorts(); len(got) != 1 || got[0].Name != "custom_in" {
		t.Fatalf("custom input ports = %v", got)
	}
	if got := n.OutputPorts(); len(got) != 1 || got[0].Name != "custom_out" {
		t.Fatalf("custom output ports = %v", got)
	}
}

func TestScriptNodeNew_RegisteredNilPortsOverrideGenericFallback(t *testing.T) {
	nodepkg.RegisterPorts("__zero_scriptnode_ports__", nil, nil)

	n := New("zero", "__zero_scriptnode_ports__", "", nil, nil)
	if got := n.InputPorts(); got != nil {
		t.Fatalf("zero input ports = %v, want nil", got)
	}
	if got := n.OutputPorts(); got != nil {
		t.Fatalf("zero output ports = %v, want nil", got)
	}
}

func TestScriptNodeNew_GlobalRegistryOverridesBuiltinCatalog(t *testing.T) {
	t.Cleanup(registerBuiltinPorts)
	nodepkg.RegisterPorts("router", []graph.Port{
		{Name: "custom_global_in", Type: graph.PortTypeString},
	}, []graph.Port{
		{Name: "custom_global_out", Type: graph.PortTypeBool},
	})

	n := New("router", "router", "", nil, nil)
	if got := n.InputPorts(); len(got) != 1 || got[0].Name != "custom_global_in" || got[0].Type != graph.PortTypeString {
		t.Fatalf("router input ports = %v, want custom global input port", got)
	}
	if got := n.OutputPorts(); len(got) != 1 || got[0].Name != "custom_global_out" || got[0].Type != graph.PortTypeBool {
		t.Fatalf("router output ports = %v, want custom global output port", got)
	}
}

func TestScriptNodeNew_UnknownTypeUsesGenericPorts(t *testing.T) {
	n := New("u1", "__unknown_scriptnode_type__", "", nil, nil)
	if got := n.InputPorts(); len(got) != 1 || got[0].Name != "input" {
		t.Fatalf("unknown input ports = %v, want generic input", got)
	}
	if got := n.OutputPorts(); len(got) != 1 || got[0].Name != "output" {
		t.Fatalf("unknown output ports = %v, want generic output", got)
	}
}

type expectedPorts struct {
	input  []graph.Port
	output []graph.Port
}

func expectedBuiltinPorts() map[string]expectedPorts {
	return map[string]expectedPorts{
		"router": {
			input:  []graph.Port{{Name: "routes", Type: graph.PortTypeAny, Required: true}},
			output: []graph.Port{{Name: "route_target", Type: graph.PortTypeString, Required: true}},
		},
		"ifelse": {
			input:  []graph.Port{{Name: "conditions", Type: graph.PortTypeAny, Required: true}},
			output: []graph.Port{{Name: "branch_result", Type: graph.PortTypeString, Required: true}},
		},
		"template": {
			input:  []graph.Port{{Name: "template", Type: graph.PortTypeString, Required: true}},
			output: []graph.Port{{Name: "template_output", Type: graph.PortTypeString, Required: true}},
		},
		"answer": {
			input: []graph.Port{
				{Name: "template", Type: graph.PortTypeString},
				{Name: "keys", Type: graph.PortTypeAny},
			},
			output: []graph.Port{{Name: "answer", Type: graph.PortTypeString, Required: true}},
		},
		"assigner": {
			input: []graph.Port{{Name: "assignments", Type: graph.PortTypeAny, Required: true}},
		},
		"loopguard": {
			output: []graph.Port{
				{Name: "loop_count", Type: graph.PortTypeInteger, Required: true},
				{Name: "loop_count_exceeded", Type: graph.PortTypeBool, Required: true},
			},
		},
		"aggregator": {
			input:  []graph.Port{{Name: "input_keys", Type: graph.PortTypeAny, Required: true}},
			output: []graph.Port{{Name: "aggregated", Type: graph.PortTypeAny}},
		},
		"gate": {
			input: []graph.Port{{Name: "commands", Type: graph.PortTypeAny, Required: true}},
			output: []graph.Port{
				{Name: "gate_result", Type: graph.PortTypeString, Required: true},
				{Name: "gate_result_output", Type: graph.PortTypeString, Required: true},
			},
		},
		"context": {
			input: []graph.Port{
				{Name: "files", Type: graph.PortTypeAny},
				{Name: "commands", Type: graph.PortTypeAny},
			},
		},
		"approval": {
			input:  []graph.Port{{Name: "prompt", Type: graph.PortTypeString}},
			output: []graph.Port{{Name: "approval_status", Type: graph.PortTypeString, Required: true}},
		},
		"iteration": {
			input: []graph.Port{
				{Name: "items", Type: graph.PortTypeArray, Required: true},
				{Name: "body_script", Type: graph.PortTypeString, Required: true},
			},
			output: []graph.Port{{Name: "iteration_results", Type: graph.PortTypeArray, Required: true}},
		},
	}
}
