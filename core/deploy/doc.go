// Package deploy is the document and assembly layer: it turns a deployment
// document into Specs and holds the kind tables a host registers into. The
// process-level Manager, Session and host bindings live in core/agent-host.
//
// It registers no implementation of its own. A registration ships beside the
// thing it registers — deploy/engines/graph, deploy/engines/a2a, and
// deploy/protocols/a2a for the exposure — so this package's import closure is the
// document format and nothing beyond it, and a host links exactly the engines it
// deploys. deploy/builtin wires the whole set for a host that wants it.
//
// The built-in exposure is A2A: deploy/protocols/a2a holds the server and the
// [ProtocolKind] that mounts it, which is possible there because the server takes
// a session table, store and identity as values rather than reaching into this
// package. The client half — a remote A2A agent used as an engine — is
// core/agent-a2a, registered by deploy/engines/a2a.
//
// The session table is not here: core/agent owns the run slot, the
// lifecycle and the checkpoint identity, and deploy wires it. Its surface is the
// protocol's — Run one message, State, Cancel — because a message is a request,
// not an entry in a backlog.
package deploy
