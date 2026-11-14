// Package agent defines what an agent run is and the table of runs an
// application serves.
//
// It owns no model call and no tool execution: an engine implements Runner (the
// four verbs a Task needs — advance, cancel, release, and report state), and
// Agent is the session table around it. Nothing here imports a runtime; the context a
// run executes under comes from the host through AgentOptions.RunContext.
//
// # The shapes
//
// Task is the session-level projection a run is checkpointed as: its
// identity and state carry the names the A2A protocol uses, because a deployment
// exposes agents through that protocol, while Round/Pending are what a step-wise
// engine needs to continue. History is the transcript — a system prompt is a
// message in it, not a field beside it; Artifacts are references to bytes the
// store holds.
//
// Event is a closed set. A consumer can switch over every kind, and the compiler
// keeps that switch honest when a kind is added. A non-streaming caller folds the
// sequence; a streaming one forwards it.
//
// Store is the durable side: a Task projection, an append-only event log, and
// content-addressed blobs. What a store must guarantee is written down once, in
// the conformance suite at core/agent/agenttest, so a second backend is verified
// against the contract instead of re-deriving it.
//
// # What is deliberately absent
//
// There is no mailbox, no queue and no steering mode. A message is handled by
// the call that carries it: Runner.Run starts a Task, Agent.Run handles one
// message (starting it, or answering the question it paused on), and a
// session that already has a run in flight is refused rather than
// buffered. That is the protocol's model too — A2A refuses a message to a
// running task with "task execution is already in progress" and offers only
// cancel. A host that wants to queue input queues it in its own application, not
// in the framework.
package agent
