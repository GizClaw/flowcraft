// Package tool provides the tool system for LLM function-calling:
// the Tool contract, a Catalog/Registry directory, and the Executor
// that dispatches calls through a middleware chain.
//
// The layering is deliberate: Registry only answers "what tools exist"
// (plus scope metadata); Executor only answers "how a call runs".
// Every cross-cutting execution policy — recovery, telemetry,
// concurrency, timeout, rate limit, approval, audit — is middleware
// (see the middleware subpackage), declared once at Executor
// construction and applied uniformly to every call.
package tool

import (
	"context"
)

// Tool is the interface that LLM-callable tools must implement.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments string) (string, error)
}

// ToolMeta carries optional, sandbox-relevant metadata about a Tool.
//
// All fields are advisory; a zero ToolMeta means "no claims, treat
// conservatively" (no rate limit, assume the tool may mutate state so
// retries are unsafe).
//
// The shape is intentionally minimal — only fields a pod-side sandbox
// can act on today are present:
//
//   - RateLimit drives request-per-second throttling middleware.
//   - MutatesState gates whether retry-on-failure logic may safely
//     re-invoke the tool with the same arguments.
//   - SelfTimeout opts the tool out of the timeout middleware's
//     default deadline.
//
// Network / filesystem / cost claims were deliberately deferred:
// the in-process runtime cannot enforce process-boundary isolation,
// and $-denominated cost caps presuppose an LLM pricing catalog
// that is also deferred.
// Add fields here when (and only when) a concrete sandbox component
// is ready to consume them.
type ToolMeta struct {
	// RateLimit is the maximum number of executions per second this
	// tool can sustain. Zero means "no claim" (no rate limit applied).
	// A negative value is treated as zero.
	RateLimit float64

	// MutatesState declares that this tool has side effects beyond
	// returning a result (writes, posts, sends mail, ...). Conservative
	// callers (retry middleware, "redo last call" prompts) should
	// refuse to re-invoke a MutatesState tool with the same input
	// without explicit user confirmation.
	//
	// Zero value (false) is the conservative default in the *opposite*
	// direction: callers that don't know better should assume the tool
	// MAY mutate state. Tools that are provably side-effect free should
	// declare MutatesState=false explicitly via Metadata().
	MutatesState bool

	// SelfTimeout declares that the tool already bounds its own
	// execution time, so the timeout middleware should not impose its
	// default deadline on top. Tools that carry their own transport
	// timeout and honour the caller's context — an RPC to a remote
	// server, for instance — set this to avoid two competing deadlines
	// where the outer one reports a less useful error.
	//
	// A per-tool override in the middleware's own table still wins:
	// this is the tool's claim, not a veto over host policy.
	SelfTimeout bool
}

// ToolMetadata is an optional interface a Tool may implement to
// declare sandbox-relevant metadata. Tools that do not implement
// this interface are treated as if they returned a zero ToolMeta
// (no rate limit, side-effects unknown).
type ToolMetadata interface {
	Metadata() ToolMeta
}

// MetadataOf returns the ToolMeta declared by t, or a zero ToolMeta
// if t does not implement ToolMetadata. Safe to call on any Tool,
// including nil-interface values (returns zero ToolMeta).
func MetadataOf(t Tool) ToolMeta {
	if t == nil {
		return ToolMeta{}
	}
	if m, ok := t.(ToolMetadata); ok {
		return m.Metadata()
	}
	return ToolMeta{}
}

// FuncTool wraps a plain function as a Tool.
func FuncTool(def Definition, fn func(ctx context.Context, args string) (string, error)) Tool {
	return &funcTool{def: def, fn: fn}
}

type funcTool struct {
	def Definition
	fn  func(ctx context.Context, args string) (string, error)
}

func (f *funcTool) Definition() Definition { return f.def }

func (f *funcTool) Execute(ctx context.Context, arguments string) (string, error) {
	return f.fn(ctx, arguments)
}
