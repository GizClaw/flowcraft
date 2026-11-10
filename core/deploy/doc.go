// Package deploy owns deployed agents: their runtimes, their sessions, and
// their lifetime.
//
// It sits above core/agent and core/runtime and knows nothing about how a client
// reaches an agent. Its unit is one deployed agent: a runtime built from one
// configuration, an engine kind that builds a runner per session, and the
// protocol that exposes it.
//
// The session table is not here: core/agent owns the run slot, the
// lifecycle and the checkpoint identity, and deploy wires it. Its surface is the
// protocol's — Run one message, State, Cancel — because a message is a request,
// not an entry in a backlog.
package deploy
