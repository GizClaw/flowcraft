// Package deploy owns deployed agents: their runtimes, their sessions, and
// their lifetime.
//
// It sits above core/agent and core/runtime. Its unit is one deployed agent: a
// runtime built from one configuration, an engine kind that builds a runner per
// session, and the protocol that exposes it.
//
// The built-in exposure is A2A: the server lives in deploy/a2aprotocol, which
// takes a session table, store and identity as values; deploy only registers it.
// The client half — a remote A2A agent used as an engine — is core/agent-a2a.
//
// The session table is not here: core/agent owns the run slot, the
// lifecycle and the checkpoint identity, and deploy wires it. Its surface is the
// protocol's — Run one message, State, Cancel — because a message is a request,
// not an entry in a backlog.
package deploy
