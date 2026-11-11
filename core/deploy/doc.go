// Package deploy is the document and assembly layer: it turns a deployment
// document into Specs and registers the engine and protocol kinds a host can
// build. The process-level Manager, Session and host bindings live in
// core/agent-host.
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
