package tool

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
)

var (
	toolMeter = telemetry.MeterWithSuffix("tool")

	toolExecCount, _    = toolMeter.Int64Counter("executions.total", metric.WithDescription("Total tool executions"))
	toolExecDuration, _ = toolMeter.Float64Histogram("duration.seconds", metric.WithDescription("Tool execution duration"))
	toolErrorCount, _   = toolMeter.Int64Counter("errors.total", metric.WithDescription("Total tool execution errors"))
)

const (
	defaultMaxConcurrency = 10
	defaultExecTimeout    = 30 * time.Second
)

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithMaxConcurrency sets the maximum number of concurrent tool executions.
func WithMaxConcurrency(n int) RegistryOption {
	return func(r *Registry) {
		if n > 0 {
			r.maxConcurrency = int64(n)
		}
	}
}

// WithExecTimeout sets the default timeout for individual tool executions.
func WithExecTimeout(d time.Duration) RegistryOption {
	return func(r *Registry) {
		if d > 0 {
			r.execTimeout = d
		}
	}
}

// Tool scope constants control visibility in tool_list and /api/tools.
// The scope is registry-level metadata and does NOT appear in model.ToolDefinition.
const (
	// ScopeAgent marks a tool as available to all agents (default).
	ScopeAgent = "agent"
	// ScopePlatform marks a tool as internal to the CoPilot platform.
	// Platform tools are hidden from tool_list and the frontend ToolSelector,
	// but can still be referenced explicitly in an LLM node's tool_names.
	ScopePlatform = "platform"
)

// Registry is a thread-safe collection of Tools.
type Registry struct {
	mu             sync.RWMutex
	tools          map[string]Tool
	scopes         map[string]string
	middlewares    []Middleware
	maxConcurrency int64
	execTimeout    time.Duration
	sem            *semaphore.Weighted
}

// NewRegistry creates a new tool registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		tools:          make(map[string]Tool),
		scopes:         make(map[string]string),
		maxConcurrency: defaultMaxConcurrency,
		execTimeout:    defaultExecTimeout,
	}
	if envVal := os.Getenv("FLOWCRAFT_TOOL_CONCURRENCY"); envVal != "" {
		if n, err := strconv.ParseInt(envVal, 10, 64); err == nil && n > 0 {
			r.maxConcurrency = n
		}
	}
	if envVal := os.Getenv("FLOWCRAFT_TOOL_TIMEOUT"); envVal != "" {
		if d, err := time.ParseDuration(envVal); err == nil && d > 0 {
			r.execTimeout = d
		}
	}
	for _, opt := range opts {
		opt(r)
	}
	r.sem = semaphore.NewWeighted(r.maxConcurrency)
	return r
}

// Register adds a tool to the registry with the default scope (ScopeAgent).
func (r *Registry) Register(tool Tool) {
	r.RegisterWithScope(tool, ScopeAgent)
}

// RegisterWithScope adds a tool to the registry with the specified scope.
func (r *Registry) RegisterWithScope(tool Tool, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Definition().Name
	r.tools[name] = tool
	r.scopes[name] = scope
}

// Unregister removes a tool by name. Returns true if the tool existed.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tools[name]
	if ok {
		delete(r.tools, name)
		delete(r.scopes, name)
	}
	return ok
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// ScopeOf returns the scope of a registered tool, or ScopeAgent if not found.
func (r *Registry) ScopeOf(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.scopes[name]; ok {
		return s
	}
	return ScopeAgent
}

// Definitions returns the ToolDefinition for every registered tool (all scopes).
func (r *Registry) Definitions() []model.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]model.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition())
	}
	return defs
}

// DefinitionsByScope returns only the ToolDefinitions matching the given scope.
func (r *Registry) DefinitionsByScope(scope string) []model.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []model.ToolDefinition
	for name, t := range r.tools {
		if r.scopes[name] == scope {
			defs = append(defs, t.Definition())
		}
	}
	return defs
}

// Names returns the names of all registered tools.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return names
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Execute runs a single tool call through the registered middleware
// chain (outermost first) and the core dispatch (lookup + timeout +
// telemetry). All errors (including tool-not-found) are returned as
// ToolResult with IsError=true, so callers never need to handle a
// separate error path.
//
// Tool errors should be classified via sdk/errdefs markers
// (PolicyDenied, BudgetExceeded, RateLimit, NotFound, ...) when the
// classification matters for upstream retry / restart decisions.
// Built-in tools are expected to follow this convention; third-party
// tools are encouraged to do the same.
func (r *Registry) Execute(ctx context.Context, call model.ToolCall) model.ToolResult {
	dispatch := composeDispatch(r.coreDispatch, r.snapshotMiddlewares())
	return dispatch(ctx, call)
}

// coreDispatch is the un-decorated execution path: span/log setup,
// lookup, optional timeout, and the actual Tool.Execute call.
// Middlewares wrap this Dispatch via Registry.Use.
func (r *Registry) coreDispatch(ctx context.Context, call model.ToolCall) model.ToolResult {
	ctx, span := telemetry.Tracer().Start(ctx, fmt.Sprintf("tool.%s.execute", call.Name), trace.WithAttributes(attribute.String(telemetry.AttrToolName, call.Name)))
	defer span.End()

	nameAttr := metric.WithAttributes(attribute.String(telemetry.AttrToolName, call.Name))

	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		span.SetStatus(codes.Error, "tool not found")
		toolExecCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrToolName, call.Name),
			attribute.String("status", "error")))
		toolErrorCount.Add(ctx, 1, nameAttr)
		return model.ToolResult{
			ToolCallID: call.ID,
			Content:    fmt.Sprintf("tool %q not found", call.Name),
			IsError:    true,
		}
	}

	execCtx := ctx
	if st, ok := t.(SelfTimeouter); !ok || !st.SelfTimeout() {
		var execCancel context.CancelFunc
		execCtx, execCancel = context.WithTimeout(ctx, r.execTimeout)
		defer execCancel()
	}

	start := time.Now()
	content, err := t.Execute(execCtx, call.Arguments)
	dur := time.Since(start)

	span.SetAttributes(attribute.Float64("tool.duration_s", dur.Seconds()))
	toolExecDuration.Record(ctx, dur.Seconds(), nameAttr)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		toolExecCount.Add(ctx, 1, metric.WithAttributes(
			attribute.String(telemetry.AttrToolName, call.Name),
			attribute.String("status", "error")))
		toolErrorCount.Add(ctx, 1, nameAttr)
		telemetry.Warn(ctx, "tool execution failed",
			otellog.String(telemetry.AttrToolName, call.Name),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return model.ToolResult{
			ToolCallID: call.ID,
			Content:    err.Error(),
			IsError:    true,
		}
	}

	span.SetStatus(codes.Ok, "OK")
	toolExecCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.AttrToolName, call.Name),
		attribute.String("status", "success")))
	return model.ToolResult{
		ToolCallID: call.ID,
		Content:    content,
	}
}

// ExecuteAll runs multiple tool calls concurrently with a semaphore
// limiting parallelism. Results are returned in the same order as the input calls.
func (r *Registry) ExecuteAll(ctx context.Context, calls []model.ToolCall) []model.ToolResult {
	results := make([]model.ToolResult, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c model.ToolCall) {
			defer wg.Done()
			defer func() {
				if rv := recover(); rv != nil {
					telemetry.Error(ctx, "tool panic recovered",
						otellog.String(telemetry.AttrToolName, c.Name),
						otellog.String("panic", fmt.Sprint(rv)))
					results[idx] = model.ToolResult{
						ToolCallID: c.ID,
						Content:    fmt.Sprintf("tool %q panicked: %v", c.Name, rv),
						IsError:    true,
					}
				}
			}()

			if err := r.sem.Acquire(ctx, 1); err != nil {
				results[idx] = model.ToolResult{
					ToolCallID: c.ID,
					Content:    fmt.Sprintf("failed to acquire semaphore: %v", err),
					IsError:    true,
				}
				return
			}
			defer r.sem.Release(1)

			results[idx] = r.Execute(ctx, c)
		}(i, call)
	}

	wg.Wait()
	return results
}
