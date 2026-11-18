// Package a2a is the A2A protocol kind: this agent reachable over A2A.
//
// It holds the whole exposure — the server, the executor, the durable task store,
// and the [Protocol] registration that mounts them in a deployment.
//
// # Why one package
//
// The server is assembled from values a deployment already has: a session table,
// a checkpoint store, an identity. So it can import core/deploy and register
// itself, and nothing in core/deploy has to know it exists. Splitting the
// registration from the server was considered and dropped: the only path that
// would have paid off — a host serving A2A without the deployment layer — is
// unreachable through the kind system, because a kind is itself a core/deploy
// value.
//
// # Why it lives under protocols/
//
// The directory tree mirrors the kind tables in core/deploy's [deploy.Kinds]:
//
//	deploy/engines/graph    an engine kind
//	deploy/engines/a2a      an engine kind
//	deploy/protocols/a2a    a protocol kind
//	deploy/builtin          all of them in one call
//
// The asymmetry with deploy/engines/* is content, not shape. An engine kind is a
// registration only, because its implementation is a lower layer (core/agent-graph,
// core/agent-a2a) that must not import core/deploy. This server has no such lower
// home — it exists to be driven by a deployment — so the implementation travels
// with its kind.
//
// The package owns the A2A translation — text as one artifact per model round,
// status as the task's state, produced artifacts as fetchable parts — and the
// durable task store that keeps tasks/get answering after a restart.
package a2a
