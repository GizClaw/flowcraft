// Package node hosts the runtime-side primitives shared by every concrete
// graph node implementation: the per-type Builder registry (Factory) and
// compatibility port declarations for custom type-only nodes.
//
// Concrete node implementations live in sub-packages and register their
// builder explicitly into a Factory; this package no longer keeps any
// global default registry, schema metadata, or BuildContext.
//
// File layout:
//
//	factory.go  Factory + NodeBuilder
//	ports.go    RegisterPorts + PortsForType
package node

import (
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

// NodeBuilder constructs a graph.Node from its declarative definition.
// Build-time dependencies (LLM resolver, tool registry, script runtime,
// workspace, etc.) are captured by the closure that returns the builder
// — see graph/node/llmnode, graph/node/scriptnode, graph/node/knowledgenode.
type NodeBuilder func(def graph.NodeDefinition) (graph.Node, error)

// Factory maps node type strings to NodeBuilders and constructs node
// instances on demand. Factory is safe for concurrent use.
type Factory struct {
	mu              sync.RWMutex
	builders        map[string]NodeBuilder
	fallbackBuilder NodeBuilder
	fallbackState   map[any]FallbackRegistration
}

// FallbackRegistration records which package installed a fallback wrapper.
// Register helpers can use it to distinguish their own wrapper from a caller's
// external fallback when they are registered more than once on the same factory.
type FallbackRegistration struct {
	External    NodeBuilder
	InstalledPC uintptr
}

// NewFactory creates an empty Factory. Call RegisterBuilder (or the
// per-sub-package Register helpers like llmnode.Register, scriptnode.Register,
// knowledgenode.Register) to populate it before passing it to runner.New.
func NewFactory() *Factory {
	return &Factory{builders: map[string]NodeBuilder{}}
}

// RegisterBuilder registers builder for the given node type. Re-registering
// the same type overwrites the previous entry.
func (f *Factory) RegisterBuilder(nodeType string, builder NodeBuilder) {
	f.mu.Lock()
	f.builders[nodeType] = builder
	f.mu.Unlock()
}

// SetFallback installs a builder that handles every node type for which no
// explicit builder is registered. Pass nil to clear.
func (f *Factory) SetFallback(builder NodeBuilder) {
	f.mu.Lock()
	f.fallbackBuilder = builder
	f.mu.Unlock()
}

// Fallback returns the current fallback builder (may be nil).
func (f *Factory) Fallback() NodeBuilder {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fallbackBuilder
}

// SetFallbackRegistration stores package-specific fallback wrapper state.
// owner should be a comparable package-private value to avoid collisions.
func (f *Factory) SetFallbackRegistration(owner any, registration FallbackRegistration) {
	f.mu.Lock()
	if f.fallbackState == nil {
		f.fallbackState = map[any]FallbackRegistration{}
	}
	f.fallbackState[owner] = registration
	f.mu.Unlock()
}

// FallbackRegistration returns fallback wrapper state previously stored for owner.
func (f *Factory) FallbackRegistration(owner any) (FallbackRegistration, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	registration, ok := f.fallbackState[owner]
	return registration, ok
}

// Build constructs a single node from its declarative definition. The
// engine's two reserved types (__end__ and passthrough) are handled
// directly so callers never need to register them.
func (f *Factory) Build(def graph.NodeDefinition) (graph.Node, error) {
	switch def.Type {
	case "__end__":
		return graph.NewPassthroughNode(graph.END, "__end__"), nil
	case "passthrough":
		return graph.NewPassthroughNode(def.ID, def.Type), nil
	}

	f.mu.RLock()
	builder, ok := f.builders[def.Type]
	fb := f.fallbackBuilder
	f.mu.RUnlock()

	if ok {
		return builder(def)
	}
	if fb != nil {
		return fb(def)
	}
	return nil, fmt.Errorf("unknown node type %q for node %q", def.Type, def.ID)
}
