// Package telemetry is the instrumentation half of OpenTelemetry support: the
// accessors that produce spans, metrics and log records, plus the attribute keys
// they standardise on.
//
// # Dependency boundary
//
// It depends on the OpenTelemetry API only, never the SDK or an exporter. That is
// deliberate: a package that logs a warning must not drag a tracing backend into
// the binary. With no provider installed the API's no-ops apply, so every helper
// here is safe to call before a host has wired anything.
//
// Installation lives in the setup subpackage: telemetry/setup builds the providers,
// resources and exporters, installs them globally, and — because tracing without
// propagation joins nothing — installs the W3C propagator. A host imports it once
// at process start; libraries import this package and nothing else.
//
// # Span names
//
// Core instruments the funnel, not the leaves: one place per layer where every
// implementation must pass through. The names below are a fixed enumeration, and
// the dimension that varies (tool, model, exit code) is an attribute:
//
//	span name                    emitted by                  floor
//	host.session.open            core/host                   none
//	host.run                     core/host                   none
//	host.cancel                  core/host                   none
//	host.delegate.async          core/host                   none
//	agent.turn                   core/agent                  none
//	checkpoint.save              core/agent                  run
//	graph.step                   agent/graph                 run
//	graph.node                   agent/graph                 node
//	sandbox.exec                 core/sandbox                call
//	tool.execute                 tool/middleware             call
//	tool.load                    core/tool                   call
//	mcp.connect                  tool/mcp                    call
//	llm.route                    core/inference/route        call
//	llm.generate|embed|decide    inference/pipeline          call
//	a2a.send                     agent/a2a                   call
//
// The floor is the trace level a span is worth at, and every producer states it
// where it opens the span: [Start] compares it against the level the turn attached
// with [WithTrace]. A deployment that records less gets no span, and still gets the
// metric and the log record beside it — the level decides how much *evidence* a run
// leaves, not whether it is measured.
//
// The root of a tree — a run, a turn — belongs to the host. Core does not know
// where a turn starts, and a guessed boundary is worse than none.
//
// A span named "bash.execute" is what this rule forbids: it makes the name's
// cardinality equal to the tool count, so every dashboard has to filter by name
// instead of grouping by it: emit "tool.execute" and put the tool's name in an
// attribute.
//
// # Three rules for producers
//
//  1. One span per layer. Do not reopen the caller's span to "avoid nesting";
//     give the layer its own name and its own semantics.
//  2. One key per fact. When the inference layer lands it decides once whether a
//     fact is emitted as gen_ai.* (the OpenTelemetry conventions) or llm.*;
//     emitting both would give one fact two sources of truth.
//  3. No payloads. Spans carry identifiers, counts and classifications, never
//     prompts, completions, tool arguments, file contents or credentials. Error
//     text belongs in a log record, where the redaction rules live.
//
// # Degradation
//
// Every producer must work with no provider installed, no parent span in the
// context, no run identity attached and no level attached: a missing parent makes a
// root span, a missing identity omits the attributes rather than writing empty
// strings, and a missing level records everything — a producer does not stop
// emitting because nobody told it how much to keep.
//
// # Where the level lives
//
// [TraceLevel] is declared here, not in the agent contract, because the layers that
// open spans cannot import it: the tool registry, the inference pipeline and a
// sandbox backend all sit below `core/agent`, and `core/agent` already depends on two
// of them. The vocabulary therefore lives with the instrumentation that enforces it,
// and the deployment layers import this package for it. Nothing about the decision
// moved: a document still writes `none|run|call|node`, `deploy` still parses it, and
// the turn is still the layer that attaches the answer.
package telemetry
