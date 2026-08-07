// Package graph builds an [agent.Engine] from a declarative graph
// definition plus a registry of node types.
//
// # The four layers
//
// The package is organised as a pipeline of four layers, each with
// exactly one representation. Types from a later layer never leak back
// into an earlier one:
//
//  1. Wire — [GraphDefinition] is the serialisable document. It is
//     JSON-shaped; node configs stay opaque ([json.RawMessage]) so the
//     kernel never interprets them. Declarative authoring enters
//     through an engine's settings in sdkx/deploy or a graph file;
//     sdk/graph/config converts YAML authoring sugar to JSON with
//     sdk/config/utils. This kernel deliberately carries no YAML
//     dependency.
//  2. Registration — node behaviour is supplied per type name via
//     [RegisterType], which binds a typed config decoder and a handler
//     closure into a [Registry]. The registry is plain in-memory
//     wiring; how node types map to providers/tools is the caller's
//     business.
//  3. Build — [Build] validates the definition against the registry,
//     compiles edge/skip conditions, statically resolves declared I/O
//     roles, and returns an immutable [*Graph]. A *Graph IS an
//     [agent.Engine] — no per-run wrapper, no rebuild step.
//  4. Execution — [*Graph.Execute] advances a node frontier wave by
//     wave. Per node it resolves "${board.<name>}" references inside
//     the raw config, decodes the typed config, validates the declared
//     reads, invokes the handler, validates the declared writes, and
//     routes to the next frontier.
//
// # Nodes are functions, not objects
//
// Node behaviour is a plain function (see [NodeType]) rather than an
// interface with lifecycle methods. Node config contains variable
// references whose values only exist at execution time, so the
// resolved config is inherently per-invocation data — it is passed as
// a parameter, never stored on shared node state. A node type is
// registered once; a *Graph is built once; concurrent runs share both
// without locking or re-assembly.
//
// # Board
//
// The kernel operates directly on [agent.Board] — typed message
// channels plus untyped control vars, fully mutex-guarded, with
// snapshot/restore for checkpoints and parallel branch isolation.
// This package defines no board type of its own.
//
// # Node I/O roles
//
// Node types declare what they read and write as [Role]s in their
// [Meta] — board variables ([RoleVar]) and message channels
// ([RoleMessages]). A role's board key is either static (Role.Name) or
// bound from the node's own config field (Role.ConfigKey), e.g. an LLM
// node declaring which channel it consumes via "messages_channel".
// Roles are resolved once at [Build] time and enforced at invocation:
// required reads must exist before the handler runs, required writes
// must exist after it returns.
package graph
