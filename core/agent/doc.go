// Package agent runs the loop between a model and the tools a runtime exposes.
//
// It owns exactly two things: the loop (model, tool calls, model again) and the
// conversation state that loop advances. It does not own the substrate — model
// access, the tool catalog, the sandbox, the file boundary — which belongs to
// core/runtime, and it does not own grouping, tracing or wire formats, which
// belong to whatever drives it.
//
// One Run is one Task: a single user request, answered after as many model
// rounds as it takes. History is continuous across Tasks by construction (each
// Task starts with the transcript so far), so the tool visibility of a Task is
// derived from that history rather than cached from the previous one.
//
// Streaming is the primary interface. Run returns a sequence of events; a
// caller that wants a final answer folds them. Nothing in the loop parks state
// where the caller cannot see it, which is why there is no board, no hook table
// and no host interface here.
package agent
