package workflow

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// StreamEvent carries a streaming event emitted by a node during execution.
type StreamEvent struct {
	Type    string `json:"type"`
	NodeID  string `json:"node_id"`
	Payload any    `json:"payload,omitempty"`
}

// StreamCallback receives streaming events during execution.
type StreamCallback func(event StreamEvent)

// RuntimeOption configures NewRuntime.
type RuntimeOption func(*runtime)

// WithMemoryFactory sets the MemoryFactory used by openSession.
func WithMemoryFactory(f MemoryFactory) RuntimeOption {
	return func(rt *runtime) { rt.memoryFactory = f }
}

// WithPrepareBoard sets a custom board preparation (platform graph vars, schema, etc.).
// When nil, prepareBoard uses the generic Request + session + history path only.
func WithPrepareBoard(fn func(ctx context.Context, agent Agent, req *Request, session MemorySession, opts []RunOption) (*Board, error)) RuntimeOption {
	return func(rt *runtime) { rt.prepareBoardFn = fn }
}

// WithDependencies supplies Strategy.Build with factories and executors (flowgraph uses these).
func WithDependencies(d *Dependencies) RuntimeOption {
	return func(rt *runtime) { rt.deps = d }
}

// RunOption configures a single Run call.
type RunOption func(*RunConfig)

// RunConfig holds resolved run-level settings. Exported so that Strategy
// implementations (e.g. flowgraph) can read stream callback, max iterations, etc.
type RunConfig struct {
	History        []model.Message
	StreamCallback StreamCallback
	MaxIterations  int
}

// ApplyRunOpts resolves a slice of RunOption into a RunConfig.
func ApplyRunOpts(opts []RunOption) RunConfig {
	var c RunConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithHistory injects message history into the main channel when no Memory session is used.
// Ignored when a non-nil Memory session is opened (MemoryFactory path wins).
func WithHistory(msgs []model.Message) RunOption {
	return func(c *RunConfig) { c.History = msgs }
}

// WithStreamCallback sets a streaming event callback for the execution.
func WithStreamCallback(cb StreamCallback) RunOption {
	return func(c *RunConfig) { c.StreamCallback = cb }
}

// WithMaxIterations caps the number of graph execution steps.
func WithMaxIterations(n int) RunOption {
	return func(c *RunConfig) { c.MaxIterations = n }
}
