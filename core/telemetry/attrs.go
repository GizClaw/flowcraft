package telemetry

// This file collects the well-known OpenTelemetry attribute / log key
// names that flowcraft components emit. Centralising them here means:
//
//   - producers (core/graph, core/graph, core/tool, ...) can
//     reference one constant instead of re-typing string literals,
//     guaranteeing key parity across the codebase;
//   - consumers (dashboards, alerting, log queries) have one place to
//     learn what to filter on.
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

	// AttrAgentID identifies the agent (core/agent.Agent.ID) executing
	// the operation. Stable across runs of the same logical agent.
	AttrAgentID = "agent.id"

	// AttrTenantID identifies the tenant on whose behalf the
	// operation is running. Producers should populate this when a
	// tenant boundary is meaningful (multi-tenant SaaS deployments).
	AttrTenantID = "tenant.id"

	// ----- Execution (what unit of work) -----

	// AttrRunID identifies one engine.Run execution
	// (engine.Run.ID). Used as the routing key for engine event
	// envelopes (engine.run.<run_id>.*).
	AttrRunID = "run.id"

	// AttrParentRunID identifies the parent run when one engine.Run
	// dispatches another (multi-agent call chain). Empty for
	// top-level runs.
	AttrParentRunID = "parent.run.id"

	// AttrTaskID identifies the A2A-aligned task an operation
	// belongs to (core/agent.Request.TaskID, mirrored into
	// core/agent.Result.TaskID). Promoted by core/agent.Run into
	// engine.Run.Attributes so engines / nodes / observers can
	// recover it without reaching back through agent state.
	// Optional: empty when the upstream Request did not carry a
	// task identifier.
	AttrTaskID = "task.id"

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
	// being executed. Emitted by core/graph/runner; absent for
	// non-graph engines.
	AttrGraphName = "graph.name"

	// AttrNodeID identifies one graph node (graph.Node.ID) inside a
	// graph run. Emitted on per-node spans, metrics and log records.
	AttrNodeID = "node.id"

	// ----- Tools -----

	// AttrToolName identifies the dispatched tool (tool.Tool.Name).
	// Emitted on tool dispatch spans / metrics.
	AttrToolName = "tool.name"

	// AttrToolCallID identifies a single tool invocation
	// (model.ToolCall.ID assigned by the LLM). Use to correlate the
	// tool_call event envelope with its tool_result.
	AttrToolCallID = "tool.call_id"

	// ----- LLM -----

	// AttrLLMProvider identifies the LLM vendor / SDK family that
	// served a call ("openai", "anthropic", "bytedance", "ollama",
	// "azure", "deepseek", "minimax", "qwen", ...). The pod
	// controller filters/aggregates on this dimension to apply
	// per-provider rate limits, circuit breakers, and cost
	// tracking; producers MUST use the lowercase short token form
	// for cross-package join-ability.
	AttrLLMProvider = "llm.provider"

	// AttrLLMModel identifies the resolved LLM model name a call
	// targets. Emitted by core/inference dispatch spans.
	AttrLLMModel = "llm.model"

	// AttrLLMInputTokens / AttrLLMOutputTokens / AttrLLMTotalTokens
	// mirror the model.TokenUsage fields. Producers MUST use these
	// exact keys when reporting LLM usage so dashboards can sum
	// across packages without per-package translation rules.
	AttrLLMInputTokens  = "llm.tokens.input"
	AttrLLMOutputTokens = "llm.tokens.output"
	AttrLLMTotalTokens  = "llm.tokens.total"

	// AttrLLMCachedInputTokens mirrors model.TokenUsage.CachedInputTokens —
	// the subset of input tokens served from the provider's prompt
	// cache. It is always a subset of AttrLLMInputTokens (enforced
	// by the adapter normalisation in backends/llm) so dashboards can
	// compute a uniform hit-rate as cached / input without
	// provider-specific branching. Producers MUST omit the
	// attribute when zero (no cache hit reported, or provider
	// does not expose a cache breakdown) to match the
	// model.TokenUsage `omitempty` wire convention and keep span
	// payloads slim on the common path.
	AttrLLMCachedInputTokens = "llm.tokens.input.cached"

	// AttrLLMCostMicros is the cost of the call in micro-units of
	// the configured currency (e.g. micro-USD = USD * 1_000_000).
	// Integer math avoids float drift in cumulative budgets. Zero
	// when the host has no pricing catalog configured.
	AttrLLMCostMicros = "llm.cost.micros"

	// AttrLLMLatencyMs is the wall-clock duration of the call in
	// milliseconds.
	AttrLLMLatencyMs = "llm.latency.ms"

	// AttrLLMRequestID is the provider-assigned request identifier for
	// an inference call (e.g. DashScope's request_id or an error
	// envelope's request id). Use it to join flowcraft spans with the
	// provider's own request logs.
	AttrLLMRequestID = "llm.request.id"

	// AttrLLMResponseID is the provider-assigned identifier of the
	// response object (e.g. OpenAI Responses' response.id, Anthropic's
	// message.id, or a chat completion id). Providers that only expose
	// one identifier usually surface it here rather than in
	// AttrLLMRequestID.
	AttrLLMResponseID = "llm.response.id"

	// ----- Conversation / data scope -----

	// AttrConversationID identifies the conversation an operation
	// belongs to. Shared by conversation history and long-term memory
	// implementations, and
	// the future core controller (multi-agent pods that share a
	// conversation context). Producers MUST use this constant
	// instead of legacy snake_case "conversation_id" string literals
	// so dashboards can join across these packages by a single
	// dimension.
	AttrConversationID = "conversation.id"

	// ----- Errors -----

	// AttrErrorMessage carries the human-readable error string on
	// log records and span events. Aligned with OTel semantic-
	// conventions `exception.message` semantically, but kept under
	// the shorter `error.message` key because flowcraft logs do not
	// otherwise emit the OTel exception-event shape (no
	// `exception.type` / `exception.stacktrace`); a single canonical
	// key for the message text is enough.
	//
	// Producers MUST use this constant rather than the legacy
	// "error" key so dashboards can filter by "error.message exists"
	// uniformly. The "error" key was used inconsistently across the
	// SDK (sometimes the message, sometimes a code) — switching to
	// a single canonical name makes the intent unambiguous.
	AttrErrorMessage = "error.message"
)
