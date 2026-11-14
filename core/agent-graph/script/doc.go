// Package script binds a rule script to a graph node.
//
// The node is deliberately narrow. A script reads the state keys it declares,
// returns the state keys it declares, and nothing else: it does not speak (only a
// model node streams text), it does not own a session, and it does not observe
// the graph. What is left is a function of (state, source), which is what makes a
// rule testable, replayable and boundable.
//
// # Not the script runtime this repository removed
//
// An earlier core shipped a JavaScript runtime with a board, channels and
// publishable messages: a second execution model that could drive an agent from
// outside. This package is the opposite trade. There is no board (the graph's
// state already is one), no channel (messages live in a declared state key) and
// no event emission (a node yields events, and a rule has none to yield). A rule
// computes state; the graph decides where that state goes.
//
// # The language is a backend
//
// The node owns the contract — the config schema, the state projection, the write
// validation, the budget discipline, the error classification — and nothing about
// the language. A [Backend] compiles a source and runs it; the package ships no
// backend, so importing this package pulls no interpreter.
//
// That seam exists because backends trade different things:
//
//	starlark (core/agent-graph/script/starlark)
//	    in-process, no dependency at run time, terminates by construction
//	    (no while, no recursion) and is deterministic by construction (no
//	    clock, no randomness, no IO). It cannot bound memory.
//	a process-backed backend
//	    can bound memory and isolate faults, and pays a process round trip per
//	    call. It cannot promise the same language guarantees for free.
//
// A configuration states what it needs, and construction fails when the backend
// cannot provide it rather than silently running with less:
//
//	budget.max_steps requires Capabilities.Terminating
//	budget.memory_mib requires Capabilities.MemoryBounded
//	allow               requires Capabilities.Tools
//
// # Tools, and why a script cannot suspend
//
// A script may call host tools, but only the ones config.allow names. The node
// dispatches them through the runtime - the same middleware chain, policy checks
// and catalog view a tool node uses - and records each call, so a replay answers
// from the record instead of running the tool again.
//
// A script cannot suspend. There is no suspension in [Host], so no backend can
// offer one, and that is deliberate: a suspended node is re-entered from the
// start, and a rule that already called a tool would call it twice. A rule that
// needs the user models the question as a graph edge, not as a pause.
//
// # Budgets
//
// Steps are the primary budget because they are reproducible: the same source
// and the same state take the same number of steps on every machine, which a wall
// clock cannot promise. Context cancellation and the node's own timeout bound the
// time a step may take.
//
// Memory is an explicit non-goal of the node: a step budget bounds work, not
// allocation, so a script can still allocate one large value. A deployment that
// needs a hard memory bound must run the agent process under its own limit
// (cgroup, rlimit, container), or pick a backend that isolates and caps the
// script's process.
//
// # Errors
//
//	config, source and compile errors        Validation (authoring mistakes)
//	a script that fails while running        Aborted
//	a budget that runs out                   BudgetExceeded
//	a write the config does not declare      Validation
package script
