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
// attribute.Key / attribute.KeyValue. Producer call sites typically wrap
// them inline (`attribute.String(telemetry.AttrRunID, id)`) — wrapping
// at this layer would force an OTel SDK import on every consumer that
// only wants the canonical name (e.g. an envelope header value).
//
// Naming convention follows the OpenTelemetry semantic-conventions style
// (lowercase, dot-separated namespace) so they coexist cleanly with the
// upstream `service.*` / `host.*` / `process.*` keys that
// buildResource() already populates.
//
// # Tiers
//
// The constants fall into three groups, and the split is mechanical where it can
// be:
//
//   - emitted: a producer exists or is part of the current instrumentation plan
//     (tool.*, sandbox.*, policy.*, mcp.*, error.*, and the LLM keys that stay);
//   - reserved: the vocabulary is fixed but the component that will emit it does
//     not exist yet (agent.*, run.*, graph.*, node.id, delegation.*). Reserved
//     keys are not dead weight — they are the names an upcoming component must
//     use, so it cannot invent a second spelling;
//   - superseded: an LLM fact the OpenTelemetry GenAI conventions now cover. The
//     key stays declared for historical queries but must not be emitted;
//     semconv.go records the replacement and TestAttrsHaveOneHome enforces it.
//
// What the tiers forbid is a second name for a fact that already has one.

const (
	// ----- Identity (who is doing the work) -----

	// AttrAgentID identifies the agent (core/agent.Agent.ID) executing
	// the operation. Stable across runs of the same logical agent.
	AttrAgentID = "agent.id"

	// AttrRuntimeID identifies one assembled agent runtime (core/runtime.Runtime.ID):
	// the substrate a turn runs on, not a conversation. Two runtimes in one
	// process have different ids, so this is what separates their spans. It is
	// distinct from AttrAgentID, which names the logical agent that may outlive
	// or share a runtime.
	AttrRuntimeID = "agent.runtime.id"

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
	// stop), "canceled" (ctx cancellation), "failed" (any other
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

	// AttrToolSource identifies where a tool came from ("builtin", "mcp",
	// "plugin").
	AttrToolSource = "tool.source"

	// AttrToolLazy marks a tool whose schema was materialised on first use
	// rather than at assembly time.
	AttrToolLazy = "tool.lazy"

	// ----- Policy, sandbox, MCP -----
	//
	// These keys have named producers in the instrumentation plan but none yet;
	// they are declared ahead of the emitters so the vocabulary cannot drift.

	// AttrPolicyDecision is the outcome of a policy check: "allow" or "deny".
	AttrPolicyDecision = "policy.decision"

	// AttrPolicyReason is the stable, redacted reason for a decision
	// ("write_path_outside_writable", "tool_not_approved", ...). Never a
	// payload, never a path the user did not already supply.
	AttrPolicyReason = "policy.reason"

	// AttrRateLimitBlockedMs is how long the rate limiter made one call wait.
	// It separates "the tool was slow" from "the fleet was saturated" on the
	// same span, which a counter cannot do.
	AttrRateLimitBlockedMs = "rate_limit.blocked_ms"

	// AttrResultTruncated marks a result the middleware shortened before the
	// model saw it.
	AttrResultTruncated = "result.truncated"

	// AttrResultDroppedRunes is how much text the limiter removed, so a
	// degraded answer is traceable to a context budget rather than to a bad
	// model.
	AttrResultDroppedRunes = "result.truncated.runes"

	// AttrResultSpilledBytes is the size of the payload persisted for the
	// model to read back after truncation.
	AttrResultSpilledBytes = "result.spilled.bytes"

	// AttrSandboxExitCode is the process exit status of an executed command.
	AttrSandboxExitCode = "sandbox.exit_code"

	// AttrSandboxRunner identifies the runner that executed a command
	// (local, bwrap, seatbelt, windows, ...), so a latency or failure
	// difference between backends is visible.
	AttrSandboxRunner = "sandbox.runner"

	// AttrSandboxTTY marks a pseudo-terminal session: it changes which
	// streams exist and whether output is line-buffered.
	AttrSandboxTTY = "sandbox.tty"

	// AttrSandboxTimeoutMs is the deadline the caller imposed on one command.
	// A zero value means none and is omitted.
	AttrSandboxTimeoutMs = "sandbox.timeout_ms"

	// AttrMCPServer identifies the MCP server a connection belongs to.
	AttrMCPServer = "mcp.server"

	// AttrInferenceShape distinguishes a unary call from a streaming one
	// ("unary" / "stream").
	AttrInferenceShape = "inference.shape"

	// AttrInferenceOperation names the seam a call belongs to
	// ("generate" / "embed" / "decide"). It is the route/metric dimension,
	// kept distinct from gen_ai.operation.name because a route span must not
	// carry gen_ai.* (a failover would then look like two model calls).
	AttrInferenceOperation = "inference.operation"

	// AttrInferenceErrorDetail is the stable, redacted stage label carried by
	// inference.Error.Detail ("stream.finish.missing"). It never holds
	// prompts, credentials or wire payloads, which is why it is safe on a span
	// while the wrapped cause is not.
	AttrInferenceErrorDetail = "inference.error.detail"

	// ----- Route (selection, retry, failover) -----

	// AttrRouteTarget is the candidate a route attempt addressed.
	AttrRouteTarget = "route.target"

	// AttrRouteSelected is the first candidate the selector returned.
	AttrRouteSelected = "route.selected"

	// AttrRouteExecuted is the candidate that answered. Empty when every
	// candidate failed.
	AttrRouteExecuted = "route.executed"

	// AttrRouteAttempts is how many provider attempts one routed call made.
	AttrRouteAttempts = "route.attempts"

	// AttrRouteRetries is how many of those attempts re-tried the same target.
	AttrRouteRetries = "route.retries"

	// AttrRouteFallbacks is how many times the dispatcher moved to another
	// target.
	AttrRouteFallbacks = "route.fallbacks"

	// AttrRouteOutcome is one attempt's result: "success" or "error".
	AttrRouteOutcome = "route.outcome"

	// AttrRouteBackoffMs is the delay a retry waited before its attempt.
	AttrRouteBackoffMs = "route.backoff_ms"

	// AttrRouteAttemptNumber is which attempt within one target this was, so a
	// reader can order the route.attempt events.
	AttrRouteAttemptNumber = "route.attempt.number"

	// AttrErrorRetryAfterMs is a server-provided backoff hint, so a consumer
	// can tell "we chose to wait" from "the provider told us to".
	AttrErrorRetryAfterMs = "error.retry_after_ms"

	// ----- LLM -----
	//
	// An LLM fact is emitted under exactly one namespace, and semconv.go records
	// which: a fact the OpenTelemetry GenAI conventions define is emitted as
	// gen_ai.* (the AttrGenAI* constants), a fact they do not define stays here.
	// The superseded keys below MUST NOT be emitted by a producer — they remain
	// declared because the constant names are part of the historical contract
	// and existing queries still reference them.

	// AttrLLMProvider is superseded by AttrGenAIProviderName. Use the lowercase
	// provider token ("openai", "anthropic", "azure.ai.openai", ...): it is
	// the dimension per-provider rate limits, circuit breakers and cost
	// tracking bucket on.
	AttrLLMProvider = "llm.provider"

	// AttrLLMModel is superseded by AttrGenAIRequestModel. The resolved model
	// that actually answered is AttrGenAIResponseModel, which can differ after
	// routing.
	AttrLLMModel = "llm.model"

	// AttrLLMInputTokens and AttrLLMOutputTokens are superseded by
	// AttrGenAIUsageInputTokens and AttrGenAIUsageOutputTokens. The input total
	// is inclusive: cached tokens and every modality are inside it.
	AttrLLMInputTokens  = "llm.tokens.input"
	AttrLLMOutputTokens = "llm.tokens.output"

	// AttrLLMTotalTokens is never emitted: it equals input+output, and the
	// conventions define no total key.
	AttrLLMTotalTokens = "llm.tokens.total"

	// AttrLLMCachedInputTokens is superseded by
	// AttrGenAIUsageCacheReadInputTokens. It is always a subset of the input
	// total; omit it when zero rather than reporting a cache miss as a hit rate
	// of zero.
	AttrLLMCachedInputTokens = "llm.tokens.input.cached"

	// AttrLLMCostMicros stays authoritative: the conventions define no cost
	// key, and the upstream naming is still contested, so squatting on
	// gen_ai.usage.cost.* now would collide when it lands. Micro-units of the
	// configured currency (micro-USD = USD * 1_000_000) keep integer math exact
	// across cumulative budgets; zero when the host has no pricing catalog.
	AttrLLMCostMicros = "llm.cost.micros"

	// AttrLLMTimeToFirstTokenMs is the delay until the first visible text
	// delta of a stream. The conventions define no such key, and it is not
	// derivable from the span duration: it separates "the model is slow to
	// start" from "the model is verbose", which are different incidents.
	// Reasoning deltas deliberately do not count.
	AttrLLMTimeToFirstTokenMs = "llm.time_to_first_token.ms"

	// AttrLLMLatencyMs is not a span attribute: the span duration already
	// carries it. It survives for callers that read usage.Usage outside a span.
	AttrLLMLatencyMs = "llm.latency.ms"

	// AttrLLMRequestID stays authoritative: gen_ai.response.id names the
	// response object, not the request id a provider support ticket asks for.
	AttrLLMRequestID = "llm.request.id"

	// AttrLLMResponseID is superseded by AttrGenAIResponseID.
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

	// ----- Delegation -----

	// AttrDelegationID identifies one delegation work item: the id
	// Submit returns and Work.ID, Response.ID and delegation_status
	// all carry. It is neither AttrRunID (the subagent turn that
	// executes the work) nor a stream escrow ref — a delegation that
	// inherits no sink has no ref, and a ref names a delivery
	// attachment rather than the work item it belongs to.
	AttrDelegationID = "delegation.id"

	// AttrDelegationTarget is the delegation target id as requested by
	// the caller (delegation.Request.Target). It can differ from
	// AttrAgentID when the directory normalizes the target to a
	// different canonical agent id.
	AttrDelegationTarget = "delegation.target"

	// AttrDelegationMode is the requested execution mode
	// (delegation.ModeSync or delegation.ModeAsync).
	AttrDelegationMode = "delegation.mode"

	// AttrDelegationDepth is the delegation nesting depth: 1 for a
	// top-level delegation, incremented per subagent hop.
	AttrDelegationDepth = "delegation.depth"

	// AttrDelegationCaller identifies the delegating agent's id;
	// empty for top-level delegations.
	AttrDelegationCaller = "delegation.caller"

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

	// AttrErrorKind is the closed classification of a failure: an
	// inference.ErrorKind on the inference path, or the tool-side
	// vocabulary ("policy_denied", "timeout", "not_found", "tool_error",
	// "panicked"). It is the dimension dashboards group failures by, so a new
	// value is a deliberate vocabulary change rather than a free-form string.
	AttrErrorKind = "error.kind"

	// AttrCodeStacktrace carries the stack captured where a panic was
	// turned into an error result. The result a caller sees says what
	// panicked, never where, so this is the only evidence a recovered
	// panic leaves behind.
	AttrCodeStacktrace = "code.stacktrace"

	// AttrErrorCause carries the leaf cause message of a failure, attached
	// only when the value is redacted by construction — flowcraft-generated
	// text (our format strings and derived scalars) plus, at most, bounded
	// identifier tokens such as tool names, tool-call ids, and media types,
	// never provider wire payloads. Error chains are diagnostic data rather
	// than redacted output, so a producer that cannot prove that property
	// must not use this key; the inference route failure log is the
	// canonical producer (see inference.CauseMessage).
	AttrErrorCause = "error.cause"
)
