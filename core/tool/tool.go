package tool

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// Tool is the interface that LLM-callable tools must implement.
//
// Execute returns provider-neutral content, not a string: a text-only
// tool returns a single text part ([message.NewTextContent]), while a
// tool that produces images, audio, files, or structured data keeps
// those parts intact all the way to the wire. Errors are transport or
// execution failures; a failure the model should read and recover from
// is better expressed as an error result at the dispatch layer, which
// preserves the distinction for middleware and telemetry.
//
// Returning zero parts with a nil error is allowed and means "nothing to
// report": dispatch normalizes it to one empty text part before the
// result is stored. Content that fails [message.Content.Validate] is a
// contract violation and comes back as an error result naming the tool.
type Tool interface {
	// Definition is what the model sees.
	Definition() message.ToolDefinition
	// Needs is what this tool must be handed, declared as the capability slots it
	// will read. Dispatch narrows the session's pocket to exactly that set and
	// nothing else, and registration refuses a tool whose needs this process cannot
	// satisfy. A tool that needs nothing returns nil, which is what makes it
	// testable with no scaffolding at all.
	Needs() []Need
	// Execute runs one call under the binding dispatch narrowed for this tool.
	Execute(ctx context.Context, b Binding, arguments string) (message.Content, error)
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

// FuncTool wraps a plain function as a Tool that needs nothing.
//
// It is the form a tool with no authority takes: registration, testing and dispatch
// all treat it as requiring zero capabilities.
func FuncTool(def message.ToolDefinition, fn func(ctx context.Context, args string) (message.Content, error)) Tool {
	return CapFunc(nil, def, func(ctx context.Context, _ Binding, args string) (message.Content, error) {
		return fn(ctx, args)
	})
}

// CapFunc wraps a function that receives the binding as a Tool declaring needs.
func CapFunc(needs []Need, def message.ToolDefinition, fn func(ctx context.Context, b Binding, args string) (message.Content, error)) Tool {
	return &funcTool{def: def, needs: needs, fn: fn}
}

type funcTool struct {
	def   message.ToolDefinition
	needs []Need
	fn    func(ctx context.Context, b Binding, args string) (message.Content, error)
}

func (f *funcTool) Definition() message.ToolDefinition { return f.def }

func (f *funcTool) Needs() []Need { return f.needs }

func (f *funcTool) Execute(ctx context.Context, b Binding, arguments string) (message.Content, error) {
	return f.fn(ctx, b, arguments)
}

// TextTool wraps a plain string-returning function as a Tool. It is the
// convenience form of [FuncTool] for the common case where the result
// is a single text part.
func TextTool(def message.ToolDefinition, fn func(ctx context.Context, args string) (string, error)) Tool {
	return FuncTool(def, func(ctx context.Context, args string) (message.Content, error) {
		text, err := fn(ctx, args)
		if err != nil {
			return message.Content{}, err
		}
		return message.NewTextContent(text), nil
	})
}

func (f *funcTool) Metadata() ToolMeta { return ToolMeta{} }

var _ Tool = (*funcTool)(nil)
var _ ToolMetadata = (*funcTool)(nil)
