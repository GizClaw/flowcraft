package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/middleware"
)

// MiddlewareFactory instantiates one middleware from its raw spec.
// Specs decode strictly: unknown fields must be an error so typos in
// YAML surface at build time, not as silently ignored policy.
type MiddlewareFactory func(ctx context.Context, spec json.RawMessage) (tool.Middleware, error)

// Deps carries the application-side dependencies YAML cannot express:
// who approves gated calls and where audit records go. A document
// using kind approval/audit without the corresponding Dep fails at
// Build, never at request time.
type Deps struct {
	Approver  middleware.Approver
	AuditSink middleware.AuditSink
}

// Builder turns validated Documents into Assemblies. It owns two named
// factory catalogs — middleware kinds, preloaded with the seven
// built-ins, and source kinds, empty by default — and binds them to one
// registry and one set of Deps. No global registration: two Builders in
// one process stay independent.
type Builder struct {
	registry        *tool.Registry
	deps            Deps
	factories       map[string]MiddlewareFactory
	sourceFactories map[string]SourceFactory
}

// NewBuilder returns a Builder over registry with the built-in
// middleware factories registered. Source kinds are opt-in via
// RegisterSourceFactory, since each pulls in an external dependency.
func NewBuilder(registry *tool.Registry, deps Deps) *Builder {
	if registry == nil {
		panic("config.NewBuilder: registry is nil")
	}
	b := &Builder{
		registry:        registry,
		deps:            deps,
		factories:       make(map[string]MiddlewareFactory),
		sourceFactories: make(map[string]SourceFactory),
	}
	b.registerBuiltins()
	return b
}

// RegisterFactory adds (or replaces) a factory for kind. Empty kinds
// and nil factories are programming bugs.
func (b *Builder) RegisterFactory(kind string, factory MiddlewareFactory) {
	if kind == "" {
		panic("config.RegisterFactory: kind is empty")
	}
	if factory == nil {
		panic(fmt.Sprintf("config.RegisterFactory: factory for kind %q is nil", kind))
	}
	b.factories[kind] = factory
}

// Assembly is the result of building a document: the Executor to
// dispatch through, plus ownership of any sources the document
// attached.
//
// It exists because sources are live resources. A build that spawned MCP
// child processes has to hand back something closeable, or those
// processes outlive the configuration that created them with no handle
// to reach them by. Close is therefore mandatory whenever a document
// declares sources, and harmless when it does not.
type Assembly struct {
	// Executor dispatches calls against the Builder's registry through
	// the document's middleware chain.
	Executor *tool.Executor

	sources []Source
}

// Close releases every source the assembly attached, unregistering
// their tools. Errors from individual sources are joined so one stuck
// child process does not hide the rest. The Executor remains usable
// afterwards but calls to a closed source's tools will fail.
func (a *Assembly) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	for _, v := range slices.Backward(a.sources) {
		if err := v.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	a.sources = nil
	return errors.Join(errs...)
}

// Build assembles the Executor.
//
// Order is deliberate. Sources attach first, so a scope or approval
// entry may reference a tool an MCP server provides — the registry is
// complete before anything reads it. Scopes are applied next, failing
// fast when config names a tool nobody registered. The middleware chain
// is instantiated last, in document order.
//
// The returned Assembly owns any attached sources and must be closed by
// the caller. A failure partway through closes whatever already
// attached, so an error never leaves child processes behind.
func (b *Builder) Build(ctx context.Context, doc Document) (*Assembly, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	sources, err := b.buildSources(ctx, doc.Sources)
	if err != nil {
		return nil, err
	}
	assembly := &Assembly{sources: sources}

	for name, scope := range doc.Scopes {
		t, ok := b.registry.Get(name)
		if !ok {
			_ = assembly.Close()
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config scopes[%q]: tool is not registered", name,
			))
		}
		b.registry.RegisterWithScope(t, scope)
	}
	chain := make([]tool.Middleware, 0, len(doc.Middlewares))
	for i, entry := range doc.Middlewares {
		factory, ok := b.factories[entry.Kind]
		if !ok {
			_ = assembly.Close()
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config middlewares[%d]: unknown kind %q", i, entry.Kind,
			))
		}
		mw, err := factory(ctx, entry.Spec)
		if err != nil {
			_ = assembly.Close()
			return nil, fmt.Errorf(
				"tool config middlewares[%d] (%s): %w", i, entry.Kind, err,
			)
		}
		chain = append(chain, mw)
	}
	assembly.Executor = tool.NewExecutor(b.registry, chain...)
	return assembly, nil
}
