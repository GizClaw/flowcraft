// Package a2a exposes a core/agent runtime as an A2A server.
//
// The protocol work — JSON-RPC, REST, gRPC, SSE, task storage, event queue — is
// the official SDK's (github.com/a2aproject/a2a-go). This package is the seam:
// it builds the AgentCard a deployment declares, and it implements
// a2asrv.AgentExecutor over agent.Session.Run.
//
// The two shapes meet unusually well: the SDK's Execute returns
// iter.Seq2[a2a.Event, error] and so does Session.Run, so the executor is a
// mapping and not a state machine.
//
// What the card may claim is deployment metadata. Streaming is a fact about the
// loop — it has no non-streaming form — and comes from the implementation.
// Media modes describe what the endpoint accepts, which is not the same question
// as what a model can ingest: a service may accept a PDF it converts to text.
// Skills are claims about what the agent can do; nothing verifies them, so they
// are declared rather than derived.
package server
