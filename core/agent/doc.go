// Package agent defines what an agent run is: the Task it advances, the events
// it reports, and the store that makes it resumable.
//
// It is a contract package. It owns no loop, no model call and no tool
// execution: an engine that answers a Task lives in its own package
// (core/agent-graph is one) and speaks these types, and core/deploy decides
// which engine a deployment uses. That is why nothing here imports a runtime.
//
// # The shapes
//
// Task is the conversation-level projection a Task is checkpointed as: its
// identity and state carry the names the A2A protocol uses, because a deployment
// exposes engines through that protocol, while Round/System/Pending are what a
// step-wise engine needs to continue. History is the transcript; Artifacts are
// references to bytes the store holds.
//
// Event is a closed set. A consumer can switch over every kind, and the compiler
// keeps that switch honest when a kind is added. A non-streaming caller folds the
// sequence; a streaming one forwards it.
//
// Store is the durable side: a Task projection, an append-only event log, and
// content-addressed blobs. What a store must guarantee is written down once, in
// the conformance suite at core/agent/agenttest, so a second backend is verified
// against the contract instead of re-deriving it.
package agent
