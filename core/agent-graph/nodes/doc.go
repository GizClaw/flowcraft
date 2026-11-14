// Package nodes provides the node kinds a graph needs to run a model loop.
//
// They are ordinary kinds, built by the host and registered like any other:
//
//	registry, _ := graph.NewRegistry(
//		nodes.Model(rt),
//		nodes.Tool(rt),
//		nodes.Condition(),
//	)
//
// Model and Tool are the two halves of a ReAct cycle: Model answers and writes the
// tool calls it wants, Tool executes them and appends their results, and edge
// conditions decide whether the cycle continues.
//
// A node may ask the caller a question by returning graph.Suspend, and the engine
// stops the run until Answer. No built-in kind does that yet: Tool executes its
// calls in one batch and cannot pause in the middle, so a graph that needs user
// input inside a tool batch has to model it with its own node kind.
//
// The kinds carry their state keys in config, because state is the only thing a
// node shares with the rest of the graph.
package nodes
