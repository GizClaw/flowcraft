// Package runner is the assembly + lifecycle layer on top of graph/executor.
//
// It turns a graph.GraphDefinition into a long-lived Runner instance: the
// definition is compiled once, then each Run call assembles fresh node
// instances and dispatches them to the configured executor. This split keeps
// graph/executor focused on the execute step alone; assembling and node
// construction belong to a higher layer because they pull in the entire node
// factory dependency graph (LLM, tool registry, script runtime, …).
package runner
