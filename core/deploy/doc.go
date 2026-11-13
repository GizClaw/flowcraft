// Package deploy is the document and assembly layer: it turns a deployment
// document into Specs and registers the engine and protocol kinds a host can
// build. The process-level Manager, Session and host bindings live in
// core/agent-host.
//
// The built-in exposure is A2A, and [BuiltinKinds] offers it: the server lives in
// deploy/a2aprotocol, which takes a session table, store and identity as values,
// while the registration ([A2AProtocol]) sits here — that package imports deploy for
// [ProtocolDeps], so a registration inside it could never be a built-in. The client
// half — a remote A2A agent used as an engine — is core/agent-a2a.
//
// The session table is not here: core/agent owns the run slot, the
// lifecycle and the checkpoint identity, and deploy wires it. Its surface is the
// protocol's — Run one message, State, Cancel — because a message is a request,
// not an entry in a backlog.
package deploy
