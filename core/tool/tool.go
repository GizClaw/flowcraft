package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// Tool is the interface that LLM-callable tools must implement.
type Tool interface {
	Definition() message.ToolDefinition
	Execute(ctx context.Context, arguments string) (string, error)
}

// ToolMeta carries optional, execution-relevant metadata about a Tool.
//
// All fields are advisory; a zero ToolMeta means "no claims, treat
// conservatively" (no rate limit, assume the tool may mutate state so
// retries are unsafe).
type ToolMeta struct {
	// RateLimit is the maximum number of executions per second this
	// tool can sustain. Zero means "no claim" (no rate limit applied).
	// A negative value is treated as zero.
	RateLimit float64

	// MutatesState declares that this tool has side effects beyond
	// returning a result (writes, posts, sends mail, ...). Zero value
	// (false) is the conservative default in the opposite direction:
	// callers that don't know better should assume the tool MAY mutate
	// state.
	MutatesState bool

	// SelfTimeout declares that the tool already bounds its own
	// execution time, so the timeout middleware should not impose its
	// default deadline on top.
	SelfTimeout bool
}

// ToolMetadata is an optional interface a Tool may implement to
// declare execution-relevant metadata. Tools that do not implement it
// are treated as if they returned a zero ToolMeta.
type ToolMetadata interface {
	Metadata() ToolMeta
}

// MetadataOf returns the ToolMeta declared by t, or a zero ToolMeta
// when t does not implement ToolMetadata. Safe on nil values.
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
func FuncTool(def message.ToolDefinition, fn func(ctx context.Context, args string) (string, error)) Tool {
	return &funcTool{def: def, fn: fn}
}

type funcTool struct {
	def message.ToolDefinition
	fn  func(ctx context.Context, args string) (string, error)
}

func (f *funcTool) Definition() message.ToolDefinition { return f.def }

func (f *funcTool) Execute(ctx context.Context, arguments string) (string, error) {
	return f.fn(ctx, arguments)
}

func (f *funcTool) Metadata() ToolMeta { return ToolMeta{} }

var _ Tool = (*funcTool)(nil)
var _ ToolMetadata = (*funcTool)(nil)
