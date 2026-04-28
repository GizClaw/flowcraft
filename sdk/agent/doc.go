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
// # Memory / history / recall integration
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
// Concrete history / recall / archival integrations are intentionally
// the caller's problem: they are 5–10 lines of glue around any
// transcript store and live with the application that owns the
// store, not in sdk/agent. See example_multiturn_test.go for the
// canonical wiring shape.
//
// # Allowed dependencies
//
//   - sdk/engine     (Engine, Host, Run, Board, Checkpoint, …)
//   - sdk/model      (Message, Part, TokenUsage)
//   - sdk/errdefs
//   - standard library
//
// agent MUST NOT import sdk/history, sdk/recall, sdk/agent/strategy
// (when added), sdk/graph, sdk/script, sdk/workflow, sdk/voice,
// sdk/event. Anything that needs an event bus (Publish wiring, OTel
// span linking, telemetry sinks) lives in the caller-supplied
// [engine.Host] (see [WithEngineHost]); agent itself does not own any
// event-routing convention.
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
//     decision-making counterpart of Observer. Round B exposes
//     BeforeFinalize for disposition / moderation; future rounds
//     will add more boundaries.
//
//  6. [DiscardOnInterruptCauses] — the canonical disposition
//     Decider for voice / streaming UX. Constructs a Decider that
//     marks Result.Committed=false on barge-in causes.
//
//  7. [Run] — the entry point that wires Request + Agent + Engine +
//     observers + deciders + seeder together for one turn, returning
//     a Result.
//
//  8. [RunOption] and the WithXxx helpers — plumb optional behaviours
//     into Run without making the function signature unwieldy.
//
// # What does NOT live here yet (round C / later)
//
//   - Honouring [FinalizeDecision.Revise] — the field is reserved on
//     the wire today; engine support and a re-execute loop come in
//     a later round.
//
//   - RunHandle / ResumeToken for in-flight run management
//     (deferred until graph adapter supplies real checkpoints).
//
//   - Strategy adapter for compiled engines (sdk/agent/strategy will host
//     it once we know what shape it should have).
//
// See docs/agent-runtime-redesign.md for the full layering rationale.
package agent
