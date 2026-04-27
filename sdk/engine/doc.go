// Package engine defines the foundational primitives shared by every
// local execution engine in FlowCraft (e.g. sdk/graph DAG executor,
// future script-based or native-Go executors).
//
// # Position in the layering
//
// engine sits below sdk/agent and below any concrete engine
// implementation. It is deliberately ignorant of agent-level
// concepts: there is no Agent, no Memory, no Request/Result, no
// chat-specific Var conventions in this package. Anything that knows
// about "agents", "messages" or "memory" belongs in sdk/agent or
// higher.
//
// Allowed dependencies:
//
//   - sdk/event       (for Envelope only; engine does NOT use Bus)
//   - sdk/errdefs     (for the interrupted-error classification)
//   - sdk/model       (for Message in Board's typed channels and
//     Part in user-prompt payloads)
//   - standard library
//
// engine MUST NOT import sdk/agent, sdk/agent/strategy, sdk/graph,
// sdk/script, sdk/history, sdk/recall, sdk/llm, sdk/tool, sdk/workflow.
//
// # The contract at a glance
//
// An engine receives three things at run time:
//
//		Execute(ctx, run Run, host Host, board *Board) (*Board, error)
//
//	  - run    — read-only metadata (ID, Attributes, Deps);
//	  - host   — capabilities the engine may invoke (Publish events,
//	             listen for Interrupts, AskUser, Checkpoint);
//	  - board  — shared blackboard the engine mutates as it runs.
//
// The Host interface is a *composition* of small interfaces:
//
//	type Host interface {
//	    Publisher       // Publish(ctx, env) error
//	    Interrupter     // Interrupts() <-chan Interrupt
//	    UserPrompter    // AskUser(ctx, prompt) (UserReply, error)
//	    Checkpointer    // Checkpoint(ctx, cp) error
//	    UsageReporter   // ReportUsage(ctx, usage)
//	}
//
// Downstream code (graph nodes, tools, …) should depend on the
// smallest interface it actually needs (Publisher alone is the
// common case) rather than the full Host. This keeps node signatures
// honest about their requirements.
//
// # What lives here
//
//  1. Board / BoardSnapshot / Cloneable — shared blackboard state and
//     typed message channels. Any engine that wants a key/value store
//     and ordered message lists reuses these.
//
//  2. Run — per-execution input bundle (ID, Attributes, Deps,
//     ResumeFrom) as a plain data struct. Setting Run.ResumeFrom is
//     how the host requests a resume; the engine interprets the
//     opaque [Checkpoint.Step] / [Checkpoint.Payload] it produced
//     earlier.
//
//  3. Host and the five small interfaces it composes — the surface
//     the engine uses to interact with its host runtime.
//
//  4. Interrupt + Cause + InterruptedError — cooperative-stop
//     primitive. Engines select on Host.Interrupts(); they convert a
//     received Interrupt into an error via [Interrupted], which
//     satisfies errdefs.IsInterrupted and carries the Cause for the
//     host to inspect via errors.As.
//
//  5. UserPrompt / UserReply — engine-agnostic, multi-modal
//     (model.Part) prompt/response payloads for input-required
//     steps.
//
//  6. Checkpoint + CheckpointStore — engine-agnostic persistence
//     contract for resumable execution. Each engine decides what its
//     own Step / Payload look like.
//
//  7. Engine — uniform Execute interface (and EngineFunc adapter) so
//     the agent layer can drive any engine through a single shape.
//
//  8. NoopHost / NoopCheckpointStore — zero-cost stand-ins for tests
//     and embedded scenarios.
//
// # What does NOT live here
//
//   - StreamCallback / StreamEvent — replaced by Publisher +
//     event.Envelope.
//   - Memory / MemorySession — that is a sdk/history + sdk/recall
//     concern at the agent layer.
//   - Strategy / Runnable / Disposition / ResumeToken — those are
//     agent ↔ engine adapter contracts and live in sdk/agent and
//     sdk/agent/strategy.
//   - VarMessages / VarQuery / VarAnswer — chat conventions that
//     belong to sdk/agent.
//   - Subject schema / engine kind enumeration — the host owns the
//     event-routing convention; engine does not reserve a namespace
//     or enumerate which engines exist.
//
// See docs/agent-runtime-redesign.md for the full layering rationale.
package engine
