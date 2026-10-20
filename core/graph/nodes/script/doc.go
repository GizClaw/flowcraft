// Package script turns a graph node's configuration into a controlled script
// execution: it registers the "script" node type, builds the global table the
// script sees, and hands execution to one of the agent's script runtimes.
//
// # Roles
//
// [NewNode] / [Register] contribute the node type; [RunScript] is the
// execution entry point a caller can drive directly; [StandardBindings] builds
// the provider that every script surface is composed from. [ScriptNodeDeps]
// is the injection point for the runtime, the board-facing capabilities and
// the host bridges — nothing is reached through globals.
//
// # Invariants
//
// A script sees only what the binding table exposes: one method per entry point
// behind a documented global (board, tools, inference, fs, host, …), each
// delegating to a host-registered capability. A script never holds a host
// object, so a capability the host did not register is a NotAvailable failure
// at call time rather than a nil dereference.
//
// The two runtimes (Lua and JavaScript, under core/agent/scriptrt) differ only
// in syntax: this package programs against the agent.ScriptRuntime contract,
// and the binding table is identical for both. Arity is part of that
// contract — a global that takes no argument must stay nullary, because the
// runtimes bind a Go func by its arity.
//
// A script failure is a node failure: the node reports it as a node error
// rather than a partial board mutation, and streams opened by the script are
// flushed at Exec exit.
//
// # Boundary
//
// The script engines, their sandboxes and their value marshalling live in
// core/agent/scriptrt; graph scheduling (including the parallel controller a
// script may call into) lives in core/graph.
package script
