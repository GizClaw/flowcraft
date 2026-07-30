// Package tool provides the tool system for LLM function-calling:
// Tool interface, Registry, concurrent execution, and schema building.
package tool

import (
	"context"
)

// Tool is the interface that LLM-callable tools must implement.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments string) (string, error)
}

// SelfTimeouter is an optional interface a Tool can implement to signal
// that it manages its own execution timeout internally (e.g. sandbox tools).
// When Registry detects this, it skips the default per-tool timeout wrapper
// so the tool's own timeout takes effect.
type SelfTimeouter interface {
	SelfTimeout() bool
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
