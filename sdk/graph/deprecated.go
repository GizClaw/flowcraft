package graph

// This file collects graph-level types and functions scheduled for removal in
// v0.3.0. Keeping them isolated lets the rest of the package evolve free of
// the legacy stream-callback model while existing callers keep compiling.
//
// Items here may import workflow because they only exist to bridge the
// workflow-era APIs; the active graph surface (vars.go, board.go, graph.go,
// stream.go, validate.go, …) does not depend on workflow.

import (
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// StreamEvent carries a streaming event emitted by a node during execution.
//
// Deprecated: prefer event.Envelope payloads delivered via event.Bus and
// produced through StreamPublisher. Aliased to workflow.StreamEvent so legacy
// callers passing workflow-typed callbacks keep compiling; scheduled for
// removal in v0.3.0 along with ExecutionContext.Stream and
// executor.WithStreamCallback.
type StreamEvent = workflow.StreamEvent

// StreamCallback receives streaming events during execution.
//
// Deprecated: use StreamPublisher (handed to nodes via ExecutionContext.Publisher)
// or subscribe directly to the configured event.Bus. Aliased to
// workflow.StreamCallback so legacy code paths interoperate without explicit
// conversion; scheduled for removal in v0.3.0.
type StreamCallback = workflow.StreamCallback
