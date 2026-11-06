package engine

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Assembly is the standard tool resource value: the aggregated
// registry (read surface), the executor (execution surface), and —
// when dynamic injection is configured — the injection policy that
// session views are created from.
type Assembly struct {
	registry *tool.Registry
	executor *tool.Executor
	policy   *Policy // nil = dynamic injection disabled
}

type assemblyOptions struct {
	conflict   tool.ConflictPolicy
	middleware []tool.Middleware
	dynamic    *Policy
}

// AssemblyOption configures NewAssembly.
type AssemblyOption func(*assemblyOptions)

// WithConflict sets the duplicate-name policy for the underlying
// registry.
func WithConflict(p tool.ConflictPolicy) AssemblyOption {
	return func(o *assemblyOptions) { o.conflict = p }
}

// WithMiddleware appends middleware to the executor chain, outermost
// first.
func WithMiddleware(mws ...tool.Middleware) AssemblyOption {
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
func NewAssembly(sources []tool.Source, opts ...AssemblyOption) (*Assembly, error) {
	o := assemblyOptions{conflict: tool.ConflictError}
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

	registry, err := tool.NewRegistry(sources, tool.WithConflictPolicy(o.conflict))
	if err != nil {
		return nil, err
	}
	mws := o.middleware
	if policy != nil {
		// Dynamic sessions keep their discovery pool fresh on every
		// executed call. The recorder is a no-op without a session on
		// the context, so one chain serves static and dynamic hosts.
		mws = append(append([]tool.Middleware{}, mws...), recordSessionCalls())
	}
	return &Assembly{
		registry: registry,
		executor: tool.NewExecutor(registry, mws...),
		policy:   policy,
	}, nil
}

// searchToolSource contributes tool_search to the registry when
// dynamic injection is enabled.
type searchToolSource struct{}

func (searchToolSource) Tools() []tool.Tool         { return []tool.Tool{SearchTool{}} }
func (searchToolSource) LazyTools() []tool.LazyTool { return nil }

// recordSessionCalls feeds every dispatched tool call back into the
// session found on the execution context so active use refreshes the
// discovery pool. Without a session on the context it is a no-op.
func recordSessionCalls() tool.Middleware {
	return func(next tool.Dispatch) tool.Dispatch {
		return func(ctx context.Context, call message.ToolCall) message.ToolResult {
			res := next(ctx, call)
			if session, ok := SessionFromContext(ctx); ok {
				session.RecordCall(call)
			}
			return res
		}
	}
}

// Catalog returns the aggregated read surface.
func (a *Assembly) Catalog() tool.Catalog { return a.registry }

// Registry is the read surface behind the assembly. A host uses it to report
// what was wired and to look tools up by name; execution goes through Execute so
// the middleware chain is not bypassed.
func (a *Assembly) Registry() *tool.Registry { return a.registry }

// Dispatcher returns the executor. Assembly itself also implements
// Dispatcher by delegation, so callers can take either form.
func (a *Assembly) Dispatcher() tool.Dispatcher { return a.executor }

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
	_ tool.Dispatcher = (*Assembly)(nil)
)
