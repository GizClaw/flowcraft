package config

import (
	"context"
	"encoding/json"
	"fmt"

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

// Builder turns validated Documents into Executors. It owns a named
// factory catalog — preloaded with the seven built-in kinds, open to
// application kinds — and binds them to one registry and one set of
// Deps. No global registration: two Builders in one process stay
// independent.
type Builder struct {
	registry  *tool.Registry
	deps      Deps
	factories map[string]MiddlewareFactory
}

// NewBuilder returns a Builder over registry with the built-in
// factories registered.
func NewBuilder(registry *tool.Registry, deps Deps) *Builder {
	if registry == nil {
		panic("config.NewBuilder: registry is nil")
	}
	b := &Builder{
		registry:  registry,
		deps:      deps,
		factories: make(map[string]MiddlewareFactory),
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

// Build assembles the Executor: scopes are applied to the registry
// first (config referencing an unregistered tool fails fast), then
// the middleware chain is instantiated in document order.
func (b *Builder) Build(ctx context.Context, doc Document) (*tool.Executor, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	for name, scope := range doc.Scopes {
		t, ok := b.registry.Get(name)
		if !ok {
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config scopes[%q]: tool is not registered", name))
		}
		b.registry.RegisterWithScope(t, scope)
	}
	chain := make([]tool.Middleware, 0, len(doc.Middlewares))
	for i, entry := range doc.Middlewares {
		factory, ok := b.factories[entry.Kind]
		if !ok {
			return nil, errdefs.Validation(fmt.Errorf(
				"tool config middlewares[%d]: unknown kind %q", i, entry.Kind))
		}
		mw, err := factory(ctx, entry.Spec)
		if err != nil {
			return nil, fmt.Errorf(
				"tool config middlewares[%d] (%s): %w", i, entry.Kind, err)
		}
		chain = append(chain, mw)
	}
	return tool.NewExecutor(b.registry, chain...), nil
}
