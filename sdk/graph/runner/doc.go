// Package runner is the assembly + lifecycle layer on top of graph/executor.
//
// It turns a graph.GraphDefinition into a long-lived Runner instance: the
// definition is compiled once, then each Run call assembles fresh node
// instances and dispatches them to the configured executor. This split keeps
// graph/executor focused on the execute step alone; assembling and node
// construction belong to a higher layer because they pull in the entire node
// factory dependency graph (LLM, tool registry, script runtime, …).
//
// # Stream-delta emission from custom nodes
//
// In-flight progress events flow through the engine-level
// SubjectStreamDelta channel — see [engine.SubjectStreamDelta] +
// [engine.StreamDeltaPayload] for the SPI. Built-in nodes (LLM, tool
// dispatch) emit token / tool_call / tool_result deltas
// automatically; custom nodes that want to surface their own progress
// have two options:
//
//  1. Use the simplified [graph.StreamPublisher] handed to every node
//     via NodeContext. Calling Emit("type", payload) goes through the
//     runner's newNodePublisher wrapper which packages the delta into
//     a SubjectStreamDelta envelope. This is the natural choice when
//     the node already lives inside graph execution.
//
//  2. For nodes that need fine-grained control over the payload
//     shape (e.g. emitting a forward-compatible Type the SDK does not
//     yet ship a helper for), use the strongly-typed helpers in the
//     engine package — [engine.EmitStreamToken],
//     [engine.EmitStreamToolCall], [engine.EmitStreamToolResult] or
//     the lower-level [engine.EmitStreamDelta]. These build the
//     envelope, attach HeaderRunID / HeaderActorID / HeaderNodeID,
//     and validate per-Type required fields before publishing, so a
//     malformed delta is caught at emit time instead of silently
//     flowing to subscribers.
//
// Both paths land on the same Subject (engine.run.<runID>.stream.
// <actorID>.delta), so consumers subscribing via
// [engine.PatternRunStream] observe LLM-emitted and node-emitted
// deltas through one subscription.
package runner
