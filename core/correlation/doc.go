// Package correlation carries the identity one run is correlated by.
//
// A run is one turn of work: a runtime handles it, a tenant pays for it, a
// session contains it, an agent performs it, a task names it and a turn numbers
// it. None of that is conversation content, and none of it is a dependency — it
// is what a span, a log record, a RunRecord and a checkpoint are joined on.
//
// The carrier is a context value ([WithRun], [From]) because the facts are
// discovered at different depths: the runtime knows its own id, the deployment
// knows the tenant and the session, the agent knows the task and the turn, and
// every component in between starts spans without knowing that runtime exists.
// A later layer overrides only the fields it sets, so a nested attachment never
// erases what an outer one established.
//
// It lives in the contracts tier, beside message and errdefs, for the same
// reason: tool, sandbox, inference, agent and delegation read the identity to
// start a span, and none of them may import the assembly that built the runtime.
// The attribute names it renders ([AttrRuntimeID] and friends) live here too, so
// "who the work belongs to" has exactly one home. core/telemetry keeps the keys
// that describe the work itself.
//
// What does not belong here: dependencies (a provider, a registry, a runner, a
// logger), conversation content (messages, transcripts), and mutable per-step
// state. The only mutable value is the precomputed attribute slice, which is
// built once when the identity is attached and never written again.
package correlation
