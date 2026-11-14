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
//	span name                    emitted by
//	llm.route                    core/inference/route   (selection, retry, failover)
//	llm.generate|embed|decide    core/inference/protocol (transport, decode, usage)
//	tool.execute                 core/tool/middleware   (one dispatched call)
//	sandbox.exec                 core/sandbox           (one executed command)
//	tool.load                    core/tool/engine       (lazy tool materialisation)
//	mcp.connect                  core/tool-mcp          (server connect / reconnect)
//
// A span named "bash.execute" is what this rule forbids: it makes the name's
// cardinality equal to the tool count, so every dashboard has to filter by name
// instead of grouping by it. Emit "tool.execute" and set AttrToolName.
//
// The root of a tree — a run, a turn — belongs to the host. Core does not know
// where a turn starts, and a guessed boundary is worse than none.
//
// # Three rules for producers
//
//  1. One span per layer. Do not reopen the caller's span to "avoid nesting";
//     give the layer its own name and its own semantics.
//  2. One key per fact. A fact covered by the OpenTelemetry GenAI conventions is
//     emitted as gen_ai.*; a fact they do not define (cost, provider request id)
//     stays in llm.*. Never both — see semconv.go and TestAttrsHaveOneHome.
//  3. No payloads. Spans carry identifiers, counts and classifications, never
//     prompts, completions, tool arguments, file contents or credentials. Error
//     text belongs in a log record, where the redaction rules live.
//
// # Degradation
//
// Every producer must work with no provider installed, no parent span in the
// context, and no run identity attached: a missing parent makes a root span, and
// a missing identity omits the attributes rather than writing empty strings. This
// is what lets core ship instrumentation before the agent layer that will own the
// root span exists.
package telemetry
