package telemetry

// This file collects the well-known OpenTelemetry attribute / log key
// names that flowcraft components emit. Centralising them here means:
//
//   - producers (sdk/engine, sdk/graph, sdk/tool, sdk/kanban, ...) can
//     reference one constant instead of re-typing string literals,
//     guaranteeing key parity across the codebase;
//   - consumers (dashboards, alerting, log queries) have one place to
//     learn what to filter on;
//   - the `sdk/pod` orchestration layer (planned) can inject pod-level
//     identity (pod.id / pod.agent) without inventing its own keys.
//
// The constants are deliberately *strings*, not typed wrappers around
// attribute.Key / otellog.KeyValue. Producer call sites typically wrap
// them inline (`attribute.String(telemetry.AttrRunID, id)`) — wrapping
// at this layer would force an OTel SDK import on every consumer that
// only wants the canonical name (e.g. an envelope header value).
//
// Naming convention follows the OpenTelemetry semantic-conventions style
// (lowercase, dot-separated namespace) so they coexist cleanly with the
// upstream `service.*` / `host.*` / `process.*` keys that
// buildResource() already populates.

const (
	// ----- Identity (who is doing the work) -----

	// AttrPodID identifies the sdk/pod runtime instance an operation
	// belongs to. Set by the pod controller; absent when the
	// operation runs outside any pod (e.g. a bare agent.Run).
	AttrPodID = "pod.id"

	// AttrAgentID identifies the agent (sdk/agent.Agent.ID) executing
	// the operation. Stable across runs of the same logical agent.
	AttrAgentID = "agent.id"

	// AttrTenantID identifies the tenant on whose behalf the
	// operation is running. Producers should populate this when a
	// tenant boundary is meaningful (multi-tenant SaaS deployments).
	AttrTenantID = "tenant.id"

	// ----- Execution (what unit of work) -----

	// AttrRunID identifies one engine.Run execution
	// (engine.Run.ID). Used as the routing key for engine event
	// envelopes (engine.run.<run_id>.*) and as the correlation
	// key in run-summary spans.
	AttrRunID = "run.id"

	// AttrParentRunID identifies the parent run when one engine.Run
	// dispatches another (multi-agent call chain). Empty for
	// top-level runs.
	AttrParentRunID = "parent.run.id"

	// AttrEngineKind identifies the concrete engine.Engine
	// implementation (graph runner, future script engine, remote
	// A2A bridge, ...). Producers SHOULD use a stable short token
	// like "graph", "script", "a2a-remote".
	AttrEngineKind = "engine.kind"

	// AttrRunStatus reports the terminal status of a run. Suggested
	// values: "ok" (clean completion), "interrupted" (cooperative
	// stop), "cancelled" (ctx cancellation), "failed" (any other
	// non-nil error). Consumers SHOULD treat unknown values as
	// "failed".
	AttrRunStatus = "run.status"

	// ----- Graph engine specifics -----

	// AttrGraphName identifies the graph definition (graph.GraphDefinition.Name)
	// being executed. Emitted by sdk/graph/runner; absent for
	// non-graph engines.
	AttrGraphName = "graph.name"

	// AttrNodeID identifies one graph node (graph.Node.ID) inside a
	// graph run. Emitted on per-node spans, metrics and log records.
	AttrNodeID = "node.id"

	// ----- Generic actor (engine-neutral) -----

	// AttrActorID is the engine-neutral identifier of the unit of
	// work that produced an event. Mirrors the engine.SubjectStep* /
	// engine.SubjectStreamDelta `actor_id` segment. Graph runner
	// sets it to the node id; script/other engines use their own
	// stable id.
	AttrActorID = "actor.id"

	// ----- Tools -----

	// AttrToolName identifies the dispatched tool (tool.Tool.Name).
	// Emitted on tool dispatch spans / metrics.
	AttrToolName = "tool.name"

	// AttrToolCallID identifies a single tool invocation
	// (model.ToolCall.ID assigned by the LLM). Use to correlate the
	// tool_call event envelope with its tool_result.
	AttrToolCallID = "tool.call_id"

	// ----- LLM -----

	// AttrLLMModel identifies the resolved LLM model name a call
	// targets. Emitted by sdk/llm dispatch spans and by the
	// run-summary span when usage is reported.
	AttrLLMModel = "llm.model"

	// AttrLLMInputTokens / AttrLLMOutputTokens / AttrLLMTotalTokens
	// mirror the model.TokenUsage fields. Producers MUST use these
	// exact keys when reporting LLM usage so dashboards can sum
	// across packages without per-package translation rules.
	AttrLLMInputTokens  = "llm.tokens.input"
	AttrLLMOutputTokens = "llm.tokens.output"
	AttrLLMTotalTokens  = "llm.tokens.total"

	// AttrLLMCostMicros is the cost of the call in micro-units of
	// the configured currency (e.g. micro-USD = USD * 1_000_000).
	// Integer math avoids float drift in cumulative budgets. Zero
	// when the host has no pricing catalog configured.
	AttrLLMCostMicros = "llm.cost.micros"

	// AttrLLMLatencyMs is the wall-clock duration of the call in
	// milliseconds.
	AttrLLMLatencyMs = "llm.latency.ms"

	// ----- Kanban -----

	// AttrKanbanCardID identifies one kanban Card (kanban.Card.ID).
	AttrKanbanCardID = "kanban.card.id"

	// AttrKanbanCardKind identifies the card kind ("task" / "signal" / ...).
	AttrKanbanCardKind = "kanban.card.kind"

	// AttrKanbanProducerID identifies the agent that produced a
	// card; mirrors kanban.WithProducerID.
	AttrKanbanProducerID = "kanban.producer.id"

	// AttrKanbanTargetAgentID identifies the consumer agent a task
	// card is targeted at.
	AttrKanbanTargetAgentID = "kanban.target.agent.id"
)
