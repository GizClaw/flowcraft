// Package agent is FlowCraft's agent abstraction: the identity, the
// execution contract, and the turn harness for one logical agent.
//
// # The shape of the abstraction
//
// An agent in FlowCraft is the triple of:
//
//	Agent  — a plain data struct: who this agent is (ID, Card, Tools)
//	Engine — the execution contract: how one run is executed
//	Run    — the per-execution input bundle: identity, attributes,
//	         dependencies, resume checkpoint
//
// wired together for one turn by [Execute]:
//
//	res, err := agent.Execute(ctx, ag, eng, req, opts...)
//
// The same [Agent] value can be driven by different engines (a graph
// DAG for rich decision trees, a script flow, an A2A-remote proxy)
// without changing its definition — engines are interchangeable
// implementations of the agent's behaviour, not part of its identity.
//
// # Position in the layering
//
//	┌──────────────────────────────────────────────────────────┐
//	│  sdk/agent                     ← this pkg                │
//	│      Agent / Request / Result / Identity                 │
//	│      Execute(...) — the turn harness                     │
//	│      Observer / Preparer / Referee — lifecycle     │
//	│      Engine / Host / Board / Run / Checkpoint — contract │
//	│                  ↑ implemented by                        │
//	│  concrete engines        sdk/graph, (script flows, …)    │
//	│  engine dependencies     sdk/script (bindings/runtime)   │
//	└──────────────────────────────────────────────────────────┘
//
// Concrete engines (sdk/graph's runner, custom inline engines built
// on [EngineFunc]) import this package and implement [Engine]. They
// reuse [Board] for shared blackboard state, [Host] for host-side
// capabilities, and [Checkpoint] for resume — they do not redefine
// them.
//
// # Identity is typed
//
// Every identity dimension of a run is a typed field on [Identity]
// (embedded in [Run]): AgentID, RunID, ParentRunID, TaskID,
// ConversationID. Envelope headers, step subjects, telemetry span
// attributes and multi-agent fan-in all read identity from that
// value — never from a stringly-typed attribute bag. [Run.Attributes]
// is reserved for non-identity metadata (tenant id, engine kind,
// feature flags) supplied via [WithAttributes].
//
// # The turn harness
//
// [Execute] is intentionally minimalist: it mints the run id, builds
// the board, dispatches lifecycle hooks, classifies the outcome into
// [Status], and assembles [Result] — nothing else. Anything that
// looks like "policy" lives on five orthogonal extension points:
//
//   - [Preparer] (via [WithPreparer]) builds the initial
//     board: load conversation history, run retrieval, materialise
//     system prompts.
//
//   - [Committer] (via [WithCommitter] or [Agent.Committers]) makes a
//     final accepted result durable and reports persistence failures.
//
//   - [Observer] (via [WithObserver] or [Agent.Observers]) reacts to run
//     lifecycle events with no return value: metrics, notifications,
//     snapshots.
//
//   - [Referee] (via [WithReferee] or [Agent.Referees])
//     influences classification at the finalize boundary: its merged
//     [Decision] drives [Result.Committed] and gates the revise loop
//     ([WithMaxRevise]).
//
//   - [Host] (via [WithHost]) is the bag of host-side capabilities
//     the engine reaches for during execution: event publishing,
//     interrupt injection, user prompting, checkpoint persistence,
//     token-usage reporting. Optional capabilities are discovered with
//     [CapabilityFromHost] through the authority boundary maintained by
//     built-in decorators. Embed [NoopHost] and override the methods you
//     need; decorate with [HostMiddleware] (e.g. [TracingMiddleware]) via
//     [ComposeHost].
//
// # Resume
//
// [WithResumeFrom](cp) replays a previous run from cp by setting
// [Run.ResumeFrom] and overriding the run id to cp.ExecID. Engines
// without [Resumer] surface NotAvailable; engines with it (graph
// runner) restore board state from cp.Board and continue from
// cp.Steps. ResumeFrom applies to attempt 1 only — revise restarts
// are fresh runs, not checkpoint replays. [LoadAndResume] packages
// the load-checkpoint-then-execute dance for hosts.
//
// # Engine assembly
//
// Engines are wired at ASSEMBLY time, not per run. An engine kind
// implements the config protocol ([config.Factory]) with its static
// dependency declaration on the spec and optional [Capabilities]
// exposed through a capability interface; sdkx/deploy registers
// engine factories alongside resource and hook factories in one
// [config.Catalog] and validates deployments before any engine
// instance exists. There is deliberately no per-run dependency bag
// on [Run] — the only per-run policy gate is the typed
// [Run.ToolAllowList], promoted from [Agent.Tools] (or overridden
// via [WithToolAllowList]).
//
// This package contains no YAML loader and no config protocol
// dependency: the config-driven assembly of agents (YAML → Factory →
// Engine) lives in sdkx/deploy, which also builds the shared
// resources an engine's deps bind to.
//
// # Allowed dependencies
//
//   - sdk/event       (Envelope subjects/headers only — no Bus)
//   - sdk/errdefs
//   - sdk/inference   (Message / Part on Board channels, Usage)
//   - sdk/telemetry   (attribute key conventions)
//   - sdk/tool        (tool definitions on Request)
//   - standard library
//
// This package MUST NOT import sdk/graph, sdk/script, memory/*, or
// any concrete engine implementation.
package agent
