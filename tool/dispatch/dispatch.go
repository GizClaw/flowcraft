// Package dispatch executes one tool call: it refuses a call without a binding,
// refuses a tool the session's grant does not allow, resolves the name against the
// process table, and invokes the tool with exactly the capabilities it declared.
//
// The refusals are the point of this package. A projection decides what the model
// *sees*; it cannot decide what the model may *call*, because a model can name any
// tool it likes. Checking the grant here — where execution happens — is what makes
// visibility a presentation decision and authority a separate one.
package dispatch

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel/attribute"
)

// Executor dispatches tool calls against a catalog through a middleware chain.
//
// The un-decorated core refuses an unbound capability, refuses a disallowed name,
// looks the tool up and invokes it; every cross-cutting concern — telemetry,
// concurrency limits, timeouts, approval, audit — is middleware, declared at
// construction. The chain is immutable afterwards, so an Executor is safe for
// concurrent use by construction.
type Executor struct {
	catalog  tool.Catalog
	dispatch tool.Dispatch
	// allows caches each tool's declared needs, which cannot change between calls.
	// Narrowing to a cached allowance is a field assignment, so the per-call path
	// allocates nothing.
	allows sync.Map // string -> tool.Allow
	allow  func(tool.Tool) tool.Allow
}

// New builds an Executor over catalog. Nil middleware values are skipped and catalog
// must be non-nil.
func New(catalog tool.Catalog, mws ...tool.Middleware) *Executor {
	if catalog == nil {
		panic("dispatch.New: catalog is nil")
	}
	e := &Executor{catalog: catalog}
	e.allow = func(t tool.Tool) tool.Allow {
		name := t.Definition().Name
		if cached, ok := e.allows.Load(name); ok {
			return cached.(tool.Allow)
		}
		allow := tool.AllowOf(t.Needs()...)
		e.allows.Store(name, allow)
		return allow
	}
	e.dispatch = compose(coreDispatch(catalog, e.allow), mws)
	return e
}

// Execute runs one call through the middleware chain. All failures — an unbound
// capability, a refused name, an unknown tool, a tool error, a panicking tool —
// surface as a result with IsError=true, never as a Go error and never as a panic,
// because the caller may be a fan-out goroutine with nowhere to recover.
func (e *Executor) Execute(ctx context.Context, b tool.Binding, request message.ToolCall) (res message.ToolResult) {
	ctx = tool.WithCallID(ctx, request.ID)
	defer func() {
		rv := recover()
		if rv == nil {
			return
		}
		telemetry.Warn(ctx, "tool dispatch panicked outside the recover middleware",
			attribute.String(telemetry.AttrToolName, request.Name),
			attribute.String(telemetry.AttrToolCallID, request.ID),
			attribute.String(telemetry.AttrErrorMessage, fmt.Sprint(rv)),
			attribute.String(telemetry.AttrCodeStacktrace, string(debug.Stack())))
		res = message.NewErrorToolResult(request.ID,
			fmt.Sprintf("tool %q panicked: %v", request.Name, rv))
	}()
	return e.dispatch(ctx, b, request)
}

// Ask asks the call's tool whether it needs an answer before it runs.
//
// It deliberately bypasses the middleware chain: asking is a read of the tool's own policy, and a
// middleware that counts, paces or audits executions must not see one that never happened. The
// authority checks are also skipped, because a refusal is reported by Execute where the call is
// actually attempted; an Ask for an unknown or refused tool simply has no question.
func (e *Executor) Ask(ctx context.Context, b tool.Binding, call message.ToolCall) (*message.Content, error) {
	if !b.Valid() || !b.Grant.Tools.Allows(call.Name) {
		return nil, nil
	}
	t, ok := e.catalog.Get(call.Name)
	if !ok {
		return nil, nil
	}
	asker, ok := t.(tool.Asker)
	if !ok {
		return nil, nil
	}
	return asker.Ask(ctx, narrow(b, e.allow(t), call.Name), call.Arguments.String())
}

// ExecuteAll runs every call concurrently through the chain and returns results in
// input order. Concurrency is unbounded by default; put a concurrency middleware in
// the chain to cap it.
func (e *Executor) ExecuteAll(ctx context.Context, b tool.Binding, requests []message.ToolCall) []message.ToolResult {
	// The fan-out only computes; this goroutine writes the results, so no goroutine
	// touches shared state.
	type outcome struct {
		index  int
		result message.ToolResult
	}
	out := make(chan outcome, len(requests))
	for i, request := range requests {
		go func(idx int, c message.ToolCall) {
			out <- outcome{index: idx, result: e.Execute(ctx, b, c)}
		}(i, request)
	}
	results := make([]message.ToolResult, len(requests))
	for range requests {
		o := <-out
		results[o.index] = o.result
	}
	return results
}

func compose(core tool.Dispatch, mws []tool.Middleware) tool.Dispatch {
	next := core
	for _, mw := range slices.Backward(mws) {
		if mw == nil {
			continue
		}
		next = mw(next)
	}
	return next
}

// coreDispatch is the chain's innermost stage: binding check, authority check, table
// lookup, the Tool.Execute call.
func coreDispatch(catalog tool.Catalog, allow func(tool.Tool) tool.Allow) tool.Dispatch {
	return func(ctx context.Context, b tool.Binding, request message.ToolCall) message.ToolResult {
		// A binding is assembled by the host and nothing else. An empty one is a
		// programming error, not "no restrictions": the policy package reads a nil
		// tool set as "every wired tool", so accepting the zero value would silently
		// widen the call.
		if !b.Valid() {
			tool.RecordFailure(ctx, tool.FailurePolicyDenied)
			return message.NewErrorToolResult(request.ID,
				"tool dispatch: the binding is unbound (no session)")
		}
		// Visibility is a projection; authority is checked here. A model can name
		// any tool it likes, so "not in the bind" has to mean "not executed", not
		// merely "not shown".
		if !b.Grant.Tools.Allows(request.Name) {
			tool.RecordFailure(ctx, tool.FailurePolicyDenied)
			return message.NewErrorToolResult(request.ID,
				errdefs.PolicyDeniedf("tool %q is not allowed by this session's policy", request.Name).Error())
		}
		t, ok := catalog.Get(request.Name)
		if !ok {
			tool.RecordFailure(ctx, tool.FailureNotFound)
			return message.NewErrorToolResult(request.ID,
				fmt.Sprintf("tool %q not found", request.Name))
		}
		// The call reached a tool the session may call, so the session's projection
		// learns it was used. Recording here rather than in middleware is what keeps a
		// refused name out of the discovery pool: a middleware wraps the whole chain,
		// so it would record the attempt, and a model naming a forbidden tool would then
		// find it visible next round and evicting tools it may actually use.
		if b.View != nil {
			b.View.RecordCall(request)
		}
		// The pocket the tool sees is narrowed one last time, to the slots it
		// declared: a capability it did not ask for reads as absent even though the
		// session holds it. The scratch space is partitioned in the same step, so two
		// tools of one session cannot collide on a key.
		scoped := narrow(b, allow(t), request.Name)
		content, err := t.Execute(ctx, scoped, request.Arguments.String())
		if err != nil {
			tool.RecordFailure(ctx, tool.ClassifyFailure(err))
			return message.NewErrorToolResult(request.ID, err.Error())
		}
		if len(content.Parts) == 0 {
			// A tool that returned no parts said nothing. Normalizing here keeps
			// every dispatched result a valid message payload, so a partless result
			// can never reach message validation or a driver's wire compile.
			content = message.NewTextContent("")
		}
		if err := content.Validate(); err != nil {
			// Content that does not validate is a broken tool, not a broken run.
			tool.RecordFailure(ctx, tool.FailureToolError)
			return message.NewErrorToolResult(request.ID,
				fmt.Sprintf("tool %q returned invalid content: %v", request.Name, err))
		}
		return message.NewToolResult(request.ID, content)
	}
}

var (
	_ tool.Dispatcher    = (*Executor)(nil)
	_ tool.AskDispatcher = (*Executor)(nil)
)

// narrow returns the binding narrowed to the needs one tool declared, with its
// scratch space partitioned by tool name.
//
// A tool that did not ask for a capability must not be able to observe it even if the
// session holds it, so the pocket is filtered rather than merely documented. The
// filter is a membership check: the session's values are never copied or nil-ed, and
// a tool that declares nothing reads nothing.
func narrow(b tool.Binding, allow tool.Allow, name string) tool.Binding {
	scoped := b.Narrow(allow)
	if scoped.Session != nil {
		scoped.Session = scopedScratch{Session: scoped.Session, prefix: name + "."}
	}
	return scoped
}

// scopedScratch prefixes every key with the tool name, so two tools of one session
// cannot collide on a key neither of them owns.
type scopedScratch struct {
	tool.Session
	prefix string
}

func (s scopedScratch) Scratch() tool.Scratch {
	inner := s.Session.Scratch()
	if inner == nil {
		return nil
	}
	return prefixedScratch{Scratch: inner, prefix: s.prefix}
}

type prefixedScratch struct {
	tool.Scratch
	prefix string
}

func (s prefixedScratch) Load(key string) (any, bool) { return s.Scratch.Load(s.prefix + key) }
func (s prefixedScratch) Store(key string, value any) { s.Scratch.Store(s.prefix+key, value) }
func (s prefixedScratch) Delete(key string)           { s.Scratch.Delete(s.prefix + key) }
