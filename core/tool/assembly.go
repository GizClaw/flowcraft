package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// Assembly is the standard tool resource value: the aggregated
// registry (read surface), the executor (execution surface), and —
// when dynamic injection is configured — the injection policy that
// session views are created from.
type Assembly struct {
	registry *Registry
	executor *Executor
	policy   *Policy // nil = dynamic injection disabled
}

type assemblyOptions struct {
	conflict   ConflictPolicy
	middleware []Middleware
	dynamic    *Policy
}

// AssemblyOption configures NewAssembly.
type AssemblyOption func(*assemblyOptions)

// WithConflict sets the duplicate-name policy for the underlying
// registry.
func WithConflict(p ConflictPolicy) AssemblyOption {
	return func(o *assemblyOptions) { o.conflict = p }
}

// WithMiddleware appends middleware to the executor chain, outermost
// first.
func WithMiddleware(mws ...Middleware) AssemblyOption {
	return func(o *assemblyOptions) {
		o.middleware = append(o.middleware, mws...)
	}
}

// WithDynamic enables dynamic tool injection with policy p. Zero
// fields fall back to DefaultPolicy; tool_search is registered with
// ExposureAlways automatically.
func WithDynamic(p Policy) AssemblyOption {
	return func(o *assemblyOptions) {
		cp := p
		o.dynamic = &cp
	}
}

// NewAssembly builds an assembly from sources. With dynamic enabled,
// the built-in tool_search tool is appended as a source so it is part
// of the registry from construction (no post-build mutation).
func NewAssembly(sources []Source, opts ...AssemblyOption) (*Assembly, error) {
	o := assemblyOptions{conflict: ConflictError}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}

	var policy *Policy
	if o.dynamic != nil {
		norm := normalizePolicy(*o.dynamic)
		if err := norm.Validate(); err != nil {
			return nil, err
		}
		if norm.Exposures == nil {
			norm.Exposures = map[string]Exposure{}
		}
		if _, ok := norm.Exposures[ToolName]; !ok {
			norm.Exposures[ToolName] = ExposureAlways
		}
		policy = &norm
		sources = append(sources, searchToolSource{})
	}

	registry, err := NewRegistry(sources, WithConflictPolicy(o.conflict))
	if err != nil {
		return nil, err
	}
	return &Assembly{
		registry: registry,
		executor: NewExecutor(registry, o.middleware...),
		policy:   policy,
	}, nil
}

// searchToolSource contributes tool_search to the registry when
// dynamic injection is enabled.
type searchToolSource struct{}

func (searchToolSource) Tools() []Tool         { return []Tool{SearchTool{}} }
func (searchToolSource) LazyTools() []LazyTool { return nil }

// Catalog returns the aggregated read surface.
func (a *Assembly) Catalog() Catalog { return a.registry }

// Dispatcher returns the executor. Assembly itself also implements
// Dispatcher by delegation, so callers can take either form.
func (a *Assembly) Dispatcher() Dispatcher { return a.executor }

// Execute implements Dispatcher.
func (a *Assembly) Execute(ctx context.Context, call message.ToolCall) message.ToolResult {
	return a.executor.Execute(ctx, call)
}

// ExecuteAll implements Dispatcher.
func (a *Assembly) ExecuteAll(ctx context.Context, calls []message.ToolCall) []message.ToolResult {
	return a.executor.ExecuteAll(ctx, calls)
}

// NewSession creates one per-run / per-conversation injection view.
// Without dynamic injection it returns a static session showing every
// tool.
func (a *Assembly) NewSession() Session {
	if a.policy == nil {
		return &staticSession{catalog: a.registry}
	}
	return &dynamicSession{
		catalog: a.registry,
		policy:  *a.policy,
		st:      *newSessionState(),
	}
}

// Close releases the registry (and any loaded deferred tools).
func (a *Assembly) Close() error {
	return a.registry.Close()
}

var (
	_ Dispatcher = (*Assembly)(nil)
)
