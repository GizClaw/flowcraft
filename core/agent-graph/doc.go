// Package graph is the execution model where an agent is a graph of nodes driven
// by a frontier.
//
// It is a sibling of core/agent, not a layer above it: agent defines what a task
// and its events are, graph runs a set of nodes whose edges decide where the run
// goes next. Both produce agent.Event streams, so deploy can expose either
// through the same protocol.
//
// # The run is a frontier loop, not a wave over a DAG
//
// A graph may loop: a ReAct cycle is model -> tools -> model. There is therefore
// no topological order and no cycle rejection. Each step invokes the frontier,
// applies the step's writes, and evaluates the outgoing edges of every node that
// ran to compute the next frontier. An edge fires when its condition holds (an
// edge with no condition always fires), so a node fans out by having several
// outgoing edges and branches join implicitly: a node reached twice in one step
// runs once. max_steps bounds a loop that never empties the frontier.
//
// # No board, and no values on edges
//
// The previous engine kept a board: untyped vars plus message channels, copied
// per branch and merged with reflection. Three things followed from that, and
// this package is built to avoid all three:
//
//   - An edge carried no meaning beyond "these two are connected", yet a wrong
//     one surfaced as a runtime type assertion with no context. Here an edge
//     carries a compiled condition over the state, so routing is explicit and a
//     bad expression fails at Build.
//   - A fan-out copied the whole board, including every message. Here nodes share
//     immutable values, and a message list is a persistent sequence: appending is
//     O(1) and shares its parent, so a branch costs its own work, not the size of
//     the history.
//   - Merging was a diff (reflect.DeepEqual plus a MergeFunc). Here state keys are
//     declared with a merge rule, and a rule that cannot decide is an error rather
//     than a silent winner.
//
// A node produces state and nothing else. Values do not travel along edges: in a
// loop, "the value on this edge" has no answer, so the state is the only carrier
// that stays well defined.
//
// # What is shared, and when
//
// Nodes in one step read the state as it was when the step started. Their writes
// are applied after the step, in declaration order. A key declared MergeSingle
// written twice in one step is an error — the rule LangGraph applies to a key with
// no reducer. A skipped node (its skip condition holds) is not invoked but its
// outgoing edges still fire: it routes without running.
//
// A node kind is a host extension point, not an external contract: it declares a
// name and how to run, and the document learns its keys when it wires them.
package graph
