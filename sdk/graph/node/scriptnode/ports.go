package scriptnode

import (
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
)

// init mirrors built-in catalog ports into node's legacy global registry so
// callers that query or override node.PortsForType("router") keep the same
// behavior they had before the catalog existed.
func init() {
	registerBuiltinPorts()
}

func registerBuiltinPorts() {
	for _, spec := range builtinCatalog {
		input, output := spec.Ports()
		node.RegisterPorts(spec.Type(), input, output)
	}
}

// portsForScriptType resolves the port shape for ScriptNode.New.
// Explicit node.RegisterPorts entries win for compatibility with the legacy
// global registry. Built-in script nodes then fall back to builtin_catalog.go,
// and unknown types use generic input/output ports.
func portsForScriptType(nodeType string) (input, output []graph.Port) {
	if input, output, ok := node.LookupPortsForType(nodeType); ok {
		return input, output
	}
	if spec, ok := builtinSpecForType(nodeType); ok {
		return spec.Ports()
	}
	return genericScriptPorts()
}

func genericScriptPorts() (input, output []graph.Port) {
	return []graph.Port{{Name: "input", Type: graph.PortTypeAny}},
		[]graph.Port{{Name: "output", Type: graph.PortTypeAny}}
}
