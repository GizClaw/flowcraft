// Package a2a is the A2A client: a remote agent as an engine.
//
// It implements agent.Runner over the A2A protocol, so a deployment can treat a
// peer exactly like a local engine. The translation is one-directional and
// lossless where the protocol allows: the peer's status updates become
// lifecycle events, its artifact chunks become answer text, and what the
// protocol has no vocabulary for (tool calls, usage) never leaves the peer.
//
// The server side — advertising a card and executing tasks — lives in
// core/deploy/protocols/a2a, which takes a session table, store and identity, and
// registers itself as the "a2a" protocol kind. This package registers nothing
// itself: deploy/engines/a2a is the registration that mounts a peer as an engine.
//
// What the two halves share is the wire mapping — a core message to A2A parts and
// back, a task state to an A2A state — and it lives in core/internal/a2awire
// rather than here. Neither half owns it: a mapping decision made twice would
// diverge, and making the server import the client's subtree would say the
// server depends on the client, which it does not.
package a2a
