package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TraceLevel is how much evidence one run leaves behind.
//
// It lives here, in the instrumentation package, because it is enforced here: the
// decision "record this span or not" is made by [Start], at the point a span is opened,
// and the packages that open those spans — the tool registry, the inference pipeline, a
// sandbox backend — sit *below* the agent contract and cannot import it (`core/tool` →
// `core/agent` → `core/inference` is a cycle, and the same holds for the other two). A
// vocabulary every producer must read cannot live above all of them.
//
// The deployment-facing half is unchanged: a document writes `none`, `run`, `call` or
// `node`, and `deploy` parses it with [ParseTraceLevel]. What moved is where the
// vocabulary is *declared*, not who decides it: the level is still a process fact
// (`runtime.observability`), attached once per turn by the layer that knows it.
//
// The cost model is per turn of an agent with one model call and two tool calls:
//
//	TraceNone   2 spans   the run and the turn
//	TraceRun    4 spans   + one per driver step
//	TraceCall   7 spans   + the model call and the two tool calls
//	TraceNode   driver-defined; the graph adds a span per node
//
// A level filters spans and nothing else. Metrics, log records, the event stream and the
// durable records a turn writes are unaffected: "how much evidence" is about the trace,
// and a deployment that keeps no inside-the-turn spans still wants its counters.
type TraceLevel int

const (
	// TraceNone records the run and its turns and nothing inside them.
	TraceNone TraceLevel = iota
	// TraceRun adds one span per driver step.
	TraceRun
	// TraceCall adds the per-call spans: a model call, a tool call, one command. It is
	// the default.
	TraceCall
	// TraceNode adds whatever a driver considers a node of its own.
	TraceNode
)

// String renders the level as the deployment document spells it.
func (l TraceLevel) String() string {
	switch l {
	case TraceNone:
		return "none"
	case TraceRun:
		return "run"
	case TraceCall:
		return "call"
	case TraceNode:
		return "node"
	default:
		return fmt.Sprintf("TraceLevel(%d)", int(l))
	}
}

// ParseTraceLevel reads the document's spelling. An unknown value is an error rather
// than a silent default: a deployment that asked for something else should hear about it
// at load time.
func ParseTraceLevel(value string) (TraceLevel, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "none", "off", "":
		return TraceNone, nil
	case "run":
		return TraceRun, nil
	case "call":
		return TraceCall, nil
	case "node":
		return TraceNode, nil
	default:
		return 0, fmt.Errorf("telemetry: unknown trace level %q (want none, run, call or node)", value)
	}
}

// AtLeast reports whether l records as much as want. A caller that holds the level itself
// — the assembly layer filtering a chain, a driver deciding whether to build a digest —
// tests it here; a producer that only opens a span passes want to [Start] instead.
func (l TraceLevel) AtLeast(want TraceLevel) bool { return l >= want }

// traceKey carries the turn's level on the context.
type traceKey struct{}

// WithTrace attaches the level every span opened below this point is filtered by.
//
// It is called once per turn, by [github.com/GizClaw/flowcraft/core/agent.RunTurn], which
// reads it from the deployment. A second attach replaces the value: a nested turn (a
// delegated run) may legitimately differ, and the innermost answer is the one in force.
func WithTrace(ctx context.Context, level TraceLevel) context.Context {
	return context.WithValue(ctx, traceKey{}, level)
}

// TraceFrom returns the level attached to ctx. ok is false when the context carries none.
func TraceFrom(ctx context.Context) (TraceLevel, bool) {
	level, ok := ctx.Value(traceKey{}).(TraceLevel)
	return level, ok
}

// Records reports whether a span worth floor may be opened under ctx.
//
// A context with no level attached records everything. That is what a command-line tool, a
// test and a library caller get, and it is the honest default: a producer does not stop
// emitting because nobody told it how much to keep. Attaching a level is what narrows it,
// and only a turn does that.
func Records(ctx context.Context, floor TraceLevel) bool {
	level, ok := TraceFrom(ctx)
	if !ok {
		return true
	}
	return level >= floor
}

// Start opens a span on tracer when the level allows it, and returns a span that is safe
// to use either way.
//
// It exists so that every producer states its own floor in one line instead of
// re-deriving the comparison, and so that a producer written later gets the turn's level
// for free — the alternative, each implementation reading the deployment document, would
// put the same decision in a dozen places. When the level forbids the span the context is
// returned unchanged and the span is a no-op, so End and AddEvent are always safe and cost
// nothing. Putting a non-recording span in the context instead would be visible to a child
// that inspects it, which is not what the level said.
func Start(
	ctx context.Context,
	tracer trace.Tracer,
	name string,
	floor TraceLevel,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if !Records(ctx, floor) {
		return ctx, noop.Span{}
	}
	return tracer.Start(ctx, name, opts...)
}
