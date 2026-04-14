package node

import (
	"context"
	"fmt"
	"io/fs"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// BuildContext provides runtime dependencies for node construction.
type BuildContext struct {
	LLMResolver   llm.LLMResolver
	ToolRegistry  *tool.Registry
	ScriptRuntime script.Runtime
	ScriptFS      fs.FS
	Workspace     workspace.Workspace
	CommandRunner workspace.CommandRunner
}

// NodeBuilder creates a graph.Node from its definition and build-time dependencies.
type NodeBuilder func(def graph.NodeDefinition, bctx *BuildContext) (graph.Node, error)

// --- Package-level default builder registry (populated by node packages via init()) ---

var (
	defaultBuildersMu      sync.RWMutex
	defaultBuilders        = map[string]NodeBuilder{}
	defaultFallbackBuilder NodeBuilder
)

// RegisterDefaultBuilder is called by node packages in init() to register
// their node type builders.
func RegisterDefaultBuilder(nodeType string, builder NodeBuilder) {
	defaultBuildersMu.Lock()
	defaultBuilders[nodeType] = builder
	defaultBuildersMu.Unlock()
}

// RegisterFallbackBuilder sets the fallback builder used when no explicit
// builder matches.
func RegisterFallbackBuilder(builder NodeBuilder) {
	defaultBuildersMu.Lock()
	defaultFallbackBuilder = builder
	defaultBuildersMu.Unlock()
}

// FactoryOption configures a Factory.
type FactoryOption func(*Factory)

func WithLLMResolver(r llm.LLMResolver) FactoryOption {
	return func(f *Factory) { f.buildCtx.LLMResolver = r }
}

func WithToolRegistry(tr *tool.Registry) FactoryOption {
	return func(f *Factory) { f.buildCtx.ToolRegistry = tr }
}

func WithScriptRuntime(rt script.Runtime) FactoryOption {
	return func(f *Factory) { f.buildCtx.ScriptRuntime = rt }
}

func WithScriptFS(fsys fs.FS) FactoryOption {
	return func(f *Factory) { f.buildCtx.ScriptFS = fsys }
}

func WithWorkspace(ws workspace.Workspace) FactoryOption {
	return func(f *Factory) { f.buildCtx.Workspace = ws }
}

func WithCommandRunner(cr workspace.CommandRunner) FactoryOption {
	return func(f *Factory) { f.buildCtx.CommandRunner = cr }
}

// Factory manages NodeBuilder registration and constructs nodes from definitions.
type Factory struct {
	mu              sync.RWMutex
	builders        map[string]NodeBuilder
	fallbackBuilder NodeBuilder
	buildCtx        *BuildContext
}

// NewFactory creates a new Factory, copying from the default builder registry.
func NewFactory(opts ...FactoryOption) *Factory {
	defaultBuildersMu.RLock()
	builders := make(map[string]NodeBuilder, len(defaultBuilders))
	for k, v := range defaultBuilders {
		builders[k] = v
	}
	fb := defaultFallbackBuilder
	defaultBuildersMu.RUnlock()

	f := &Factory{
		builders:        builders,
		fallbackBuilder: fb,
		buildCtx:        &BuildContext{},
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// RegisterBuilder registers a builder for a specific node type on this instance.
func (f *Factory) RegisterBuilder(nodeType string, builder NodeBuilder) {
	f.mu.Lock()
	f.builders[nodeType] = builder
	f.mu.Unlock()
}

// Fallback returns the current fallback builder (may be nil).
func (f *Factory) Fallback() NodeBuilder {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.fallbackBuilder
}

// SetFallback sets a builder that is tried when no explicit builder matches.
func (f *Factory) SetFallback(builder NodeBuilder) {
	f.mu.Lock()
	f.fallbackBuilder = builder
	f.mu.Unlock()
}

// Build constructs a single node from its definition.
func (f *Factory) Build(def graph.NodeDefinition) (graph.Node, error) {
	switch def.Type {
	case "__end__":
		return graph.NewPassthroughNode(graph.END, "__end__"), nil
	case "passthrough":
		return graph.NewPassthroughNode(def.ID, def.Type), nil
	default:
		f.mu.RLock()
		builder, ok := f.builders[def.Type]
		fb := f.fallbackBuilder
		f.mu.RUnlock()

		if ok {
			return builder(def, f.buildCtx)
		}
		if fb != nil {
			return fb(def, f.buildCtx)
		}
		return nil, fmt.Errorf("unknown node type %q for node %q", def.Type, def.ID)
	}
}

// ValidateConsistency checks that every registered builder has a corresponding
// schema and vice versa. Returns a list of warning messages for any mismatches.
// Intended for startup-time diagnostics, not hot paths.
func (f *Factory) ValidateConsistency(schemas *SchemaRegistry) []string {
	f.mu.RLock()
	builderTypes := make(map[string]bool, len(f.builders))
	for t := range f.builders {
		builderTypes[t] = true
	}
	f.mu.RUnlock()

	schemas.mu.RLock()
	schemaTypes := make(map[string]bool, len(schemas.schemas))
	for t := range schemas.schemas {
		schemaTypes[t] = true
	}
	schemas.mu.RUnlock()

	var warnings []string
	for t := range builderTypes {
		if !schemaTypes[t] {
			msg := fmt.Sprintf("node.factory: builder registered for type %q but no schema found", t)
			telemetry.Warn(context.Background(), msg)
			warnings = append(warnings, msg)
		}
	}
	for t := range schemaTypes {
		if t == "__end__" {
			continue
		}
		if !builderTypes[t] {
			msg := fmt.Sprintf("node.factory: schema registered for type %q but no builder found", t)
			telemetry.Warn(context.Background(), msg)
			warnings = append(warnings, msg)
		}
	}
	return warnings
}
