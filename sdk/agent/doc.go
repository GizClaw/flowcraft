// Package agent is the application-layer surface for FlowCraft agents.
//
// # Position in the layering
//
//	┌──────────────────────────────────────────────────────────┐
//	│  application layer       sdk/agent          ← this pkg   │
//	│                                                          │
//	│      ┌──────────────────────┐                            │
//	│      │  Agent / Request /   │                            │
//	│      │  Result / Observer / │                            │
//	│      │  Decider /           │                            │
//	│      │  BoardSeeder /       │                            │
//	│      │  agent.Run(...)      │                            │
//	│      └──────────────────────┘                            │
//	│                  ↓ drives                                │
//	│  core layer              sdk/engine                      │
//	│      Engine / Host / Run / Board / Checkpoint            │
//	│                  ↑ implemented by                        │
//	│  concrete engines        sdk/graph, sdk/script           │
//	└──────────────────────────────────────────────────────────┘
//
// agent owns "what the user sees": agents, conversations, request /
// result envelopes, lifecycle observation, lifecycle decisions
// (disposition, moderation), board seeding policy. It deliberately
// does NOT own "how a turn is executed" — execution is delegated to
// an [engine.Engine] passed at run time:
//
//	agent.Run(ctx, ag, eng, req, opts...)
//
// The same agent identity can be driven by different engines (graph for
// rich decision trees, script for simple flows, A2A-remote for federation)
// without changing its definition. This is the central design point that
// supersedes sdk/workflow's Agent-owns-Strategy coupling.
//
// # Transcript and retrieval integration
//
// agent does NOT define a History interface. The reason: not every
// engine speaks "graph + node" or stores its working state on the
// engine.Board the same way, so a single contract for "load before"
// and "append after" leaks engine assumptions into the application
// layer. Instead, agent exposes four orthogonal extension points:
//
//   - [BoardSeeder] (via [WithBoardSeed]) builds the initial board.
//     Use it to load conversation history, run retrieval, materialise
//     system prompts, or whatever the engine expects to find on the
//     board at start.
//
//   - [Observer] (via [WithObserver] or [Agent.Observers]) reacts to
//     run lifecycle events with no return value. Use it to append the
//     produced messages to a transcript on completion, emit metrics,
//     snapshot board state on interrupt, etc.
//
//   - [Decider] (via [WithDecider] or [Agent.Deciders]) influences
//     the run's classification at boundary points. Round B exposes
//     [Decider.BeforeFinalize], which sets [Result.Committed] —
//     transcript / archival Observers gate on that flag, so a
//     barge-in or moderation hit can opt the run out of persistence
//     without rewriting any persistence wiring.
//
//   - [engine.Host] (via [WithEngineHost]) is the bag of host-side
//     capabilities the engine reaches for during execution: event
//     publishing, interrupt injection, user prompting, checkpoint
//     persistence, token-usage reporting. Build one struct that
//     embeds [engine.NoopHost] and override the methods you actually
//     need; share metric clients / tracers / loggers across methods
//     the way only a struct can. agent never wraps or decorates the
//     supplied host — what you pass in is exactly what the engine
//     sees.
//
// Concrete transcript, retrieval, and archival integrations are
// intentionally the caller's problem: they are thin glue around the
// stores and retrievers the application owns, not built into sdk/agent.
// See example_multiturn_test.go for the canonical wiring shape.
//
// # Allowed dependencies
//
//   - sdk/engine     (Engine, Host, Run, Board, Checkpoint, …)
//   - sdk/model      (Message, Part, TokenUsage)
//   - sdk/errdefs
//   - standard library
//
// agent MUST NOT import sdk/agent/strategy (when added), sdk/graph,
// sdk/script, sdk/workflow, sdk/voice, sdk/event, or application-owned
// transcript/retrieval packages. Anything that needs an event bus
// (Publish wiring, OTel span linking, telemetry sinks) lives in the
// caller-supplied [engine.Host] (see [WithEngineHost]); agent itself
// does not own any event-routing convention.
//
// # What lives here
//
//  1. [Agent], [AgentCard], [Skill] — agent identity and capability
//     description. Agent is a *plain struct* (no Strategy-method on it):
//     execution wiring is the caller's concern.
//
//  2. [Request] / [Result] / [Status] / [Artifact] — one-turn input/output.
//
//  3. [BoardSeeder] / [BoardSeederFunc] — the data-injection extension
//     point that runs once before engine.Execute.
//
//  4. [Observer] / [BaseObserver] / [RunInfo] — the read-only
//     lifecycle hooks fired around engine.Execute.
//
//  5. [Decider] / [BaseDecider] / [FinalizeDecision] — the
//     decision-making counterpart of Observer. BeforeFinalize fires
//     after every engine.Execute attempt; the merged decision drives
//     [Result.Committed], records the [FinalizeDecision.Reason] in
//     Result.State["finalize_reason"], and (when [WithMaxRevise] is
//     enabled) gates the revise loop.
//
//  6. [DiscardOnInterruptCauses] — the canonical disposition
//     Decider for voice / streaming UX. Constructs a Decider that
//     marks Result.Committed=false on barge-in causes.
//
//  7. [Run] — the entry point that wires Request + Agent + Engine +
//     observers + deciders + seeder together for one turn, returning
//     a Result. Honours [WithResumeFrom] for checkpoint replay and
//     [WithMaxRevise] for Decider-driven re-attempts (see
//     "Resume / Revise" below).
//
//  8. [RunOption] and the WithXxx helpers — plumb optional behaviours
//     into Run without making the function signature unwieldy.
//
// # Resume / Revise
//
// Two attempt-shaping options compose with everything else:
//
//   - [WithResumeFrom](cp) replays a previous run from cp by setting
//     engine.Run.ResumeFrom and overriding the run id to cp.ExecID.
//     Engines without [engine.Resumer] surface NotAvailable; engines
//     with it (graph runner, future script engine) restore board
//     state from cp.Board and continue from cp.Step. ResumeFrom
//     applies to attempt 1 only — see Revise.
//
//   - [WithMaxRevise](n) opts in to the
//     [FinalizeDecision.Revise] loop. When n>=2, deciders that
//     return Revise=true on a completed attempt trigger another
//     engine.Execute pass with a freshly-seeded board (revise is a
//     fresh retry, not a checkpoint replay — ResumeFrom is dropped
//     after attempt 1). The loop exits when no decider asks for
//     revise OR the attempt counter reaches n. Failed /
//     interrupted / canceled / aborted attempts NEVER consume
//     budget; transient infrastructure errors surface immediately.
//     [Result.Attempts] reports the actual count;
//     [Observer.OnRunRevise] fires once per attempt transition.
//
// # What does NOT live here yet (later)
//
//   - RunHandle / ResumeToken for in-flight run management
//     (deferred until host-level handle plumbing matures).
//
//   - Strategy adapter for compiled engines (sdk/agent/strategy will host
//     it once we know what shape it should have).
package agent
