package node

import (
	"sync"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

// Port declarations for jsnode and other "type-only" nodes that don't have
// a Go-native struct to implement graph.PortDeclarable. Built-in jsnode
// types register their ports here from their owning sub-package's init().
//
// This is the entire schema surface that the engine actually needs at
// runtime. UI metadata (label, icon, fields, runtime hints) lives elsewhere
// — typically in product-specific node-catalog services that consume the
// engine but are not part of it.

var (
	portsMu sync.RWMutex
	ports   = map[string]struct {
		input, output []graph.Port
	}{}
)

// RegisterPorts declares the input/output port shape for a node type.
// Safe for concurrent use. Re-registering a type overwrites the previous
// entry. Intended to be called from init() of the owning sub-package.
func RegisterPorts(nodeType string, input, output []graph.Port) {
	portsMu.Lock()
	ports[nodeType] = struct{ input, output []graph.Port }{input: input, output: output}
	portsMu.Unlock()
}

// PortsForType returns the ports previously registered for nodeType. The
// returned slices are nil when no registration exists, mirroring the
// "unknown type" branch in scriptnode (which falls back to generic
// input/output ports).
func PortsForType(nodeType string) (input, output []graph.Port) {
	portsMu.RLock()
	defer portsMu.RUnlock()
	if p, ok := ports[nodeType]; ok {
		return p.input, p.output
	}
	return nil, nil
}
