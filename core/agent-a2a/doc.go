// Package a2a is the A2A client: a remote agent as an engine.
//
// It implements agent.Runner over the A2A protocol, so a deployment can treat a
// peer exactly like a local engine. The translation is one-directional and
// lossless where the protocol allows: the peer's status updates become
// lifecycle events, its artifact chunks become answer text, and what the
// protocol has no vocabulary for (tool calls, usage) never leaves the peer.
//
// The server side — advertising a card and executing tasks — lives in
// core/agent-a2a/server.
package a2a
