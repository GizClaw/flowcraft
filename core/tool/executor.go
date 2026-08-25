package tool

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/GizClaw/flowcraft/core/message"
)

// Dispatcher is the execution contract consumers depend on: run one
// tool call, or several concurrently. *Executor is the canonical
// implementation, but tests and adapters may substitute their own.
type Dispatcher interface {
	Execute(context.Context, message.ToolCall) message.ToolResult
	ExecuteAll(context.Context, []message.ToolCall) []message.ToolResult
}

// Dispatch is the function form of "execute one tool call and return
// its result". Middleware operates on this signature.
//
// Implementations should treat the input call as immutable and must
// always return a Result — errors are reported via
// Result.IsError, never returned out-of-band.
type Dispatch func(ctx context.Context, call message.ToolCall) message.ToolResult

// Middleware wraps a Dispatch. mws[0] is outermost (sees the call
// first and the result last), matching agent.ComposeHost.
type Middleware func(next Dispatch) Dispatch

// Executor dispatches tool calls against a Catalog through a
// middleware chain. The un-decorated core only looks the tool up and
// invokes it; every cross-cutting concern — recovery, telemetry,
// concurrency limits, timeouts, approval, audit — is middleware,
// declared at construction. The chain is immutable afterwards, so an
// Executor is safe for concurrent use by construction.
type Executor struct {
	dispatch Dispatch
}

// NewExecutor builds an Executor over catalog. Nil middleware values
// are skipped. catalog must be non-nil.
func NewExecutor(catalog Catalog, mws ...Middleware) *Executor {
	if catalog == nil {
		panic("tool.NewExecutor: catalog is nil")
	}
	return &Executor{
		dispatch: composeDispatch(coreDispatch(catalog), mws),
	}
}

// Execute runs one call through the middleware chain. All failures —
// unknown tool, tool error — surface as Result with IsError=true,
// never as a Go error.
func (e *Executor) Execute(ctx context.Context, call message.ToolCall) message.ToolResult {
	return e.dispatch(WithCallID(ctx, call.ID), call)
}

// ExecuteAll runs every call concurrently through the chain and
// returns results in input order. Concurrency is unbounded by
// default; put a concurrency middleware in the chain to cap it.
func (e *Executor) ExecuteAll(ctx context.Context, calls []message.ToolCall) []message.ToolResult {
	results := make([]message.ToolResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c message.ToolCall) {
			defer wg.Done()
			results[idx] = e.Execute(ctx, c)
		}(i, call)
	}
	wg.Wait()
	return results
}

func composeDispatch(core Dispatch, mws []Middleware) Dispatch {
	next := core
	for _, mw := range slices.Backward(mws) {
		if mw == nil {
			continue
		}
		next = mw(next)
	}
	return next
}

// coreDispatch is the chain's innermost stage: catalog lookup plus
// the Tool.Execute call. Unknown tools and tool errors both become
// IsError results; there is no out-of-band error channel.
func coreDispatch(catalog Catalog) Dispatch {
	return func(ctx context.Context, call message.ToolCall) message.ToolResult {
		t, ok := catalog.Get(call.Name)
		if !ok {
			return message.ToolResult{
				CallID:  call.ID,
				Content: fmt.Sprintf("tool %q not found", call.Name),
				IsError: true,
			}
		}
		content, err := t.Execute(ctx, string(call.Arguments))
		if err != nil {
			return message.ToolResult{
				CallID:  call.ID,
				Content: err.Error(),
				IsError: true,
			}
		}
		return message.ToolResult{CallID: call.ID, Content: content}
	}
}

var _ Dispatcher = (*Executor)(nil)
