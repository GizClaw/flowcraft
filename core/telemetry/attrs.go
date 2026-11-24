package telemetry

// This file collects the well-known OpenTelemetry attribute / log key names that
// flowcraft components emit. Centralising them means producers reference one
// constant instead of re-typing a string literal, and a consumer has one place to
// learn what to filter on.
//
// One family is deliberately not here: the identity a run carries (tenant.id,
// agent.name, session.id, agent.grant.id, task.id, turn, chain.id, chain.depth).
// It belongs to core/identity, beside the carrier that renders it, because every
// layer below the assembly reads the identity to start a span and should not have
// to depend on this package for eight strings. That package does not exist yet;
// until it does, no producer emits an identity attribute.
//
// # One rule: a key exists when its emitter does
//
// A name declared ahead of its producer is a claim nothing verifies. After a few
// months nobody can tell which of the declared keys are real, and the file stops
// answering the only question it is for — what does this process emit? So each
// batch adds its keys in the same change that adds the producer, together with the
// test that exercises them.
//
// It also keeps names honest. A reserved key is written against the architecture
// of the day it was reserved; when the architecture changes, the reservation
// survives as advice that is now wrong, and the next author copies it.
//
// # Values
//
// The constants are deliberately *strings*, not typed wrappers around
// attribute.Key / attribute.KeyValue, and values are built with this package's own
// constructors — `telemetry.String(telemetry.AttrSandboxRunner, name)`. Together
// they mean a component that logs does not import the OpenTelemetry SDK at all:
// naming a key and building its value are both local. A consumer that only wants
// the canonical name (an envelope header, a log field) imports nothing.
//
// Naming follows the OpenTelemetry semantic-conventions style (lowercase,
// dot-separated namespace) so the keys coexist cleanly with the upstream
// service.* / host.* / process.* set the resource already populates.
//
// # One namespace per fact
//
// The inference layer decides the namespace once per fact, by the rule the
// conventions state: a fact they define is emitted as gen_ai.*, and a fact they do
// not define stays in llm.*. Only the emitted spelling is declared here, so the two
// cannot both be emitted by accident — there is no second constant to reach for.
// That is stricter than declaring a superseded key and asking producers not to use
// it, and it is what the two namespaces were originally arbitrated in a table for.
//
// The GenAI registry is still development-stability, so re-check these spellings
// when the inference layer is tagged rather than assuming this snapshot still
// matches.

// SemconvGenAIVersion records the convention revision the gen_ai.* keys below were
// aligned with. Bumping it is a deliberate, reviewable change.
const SemconvGenAIVersion = "1.37.0"

const (
	// ----- Command execution -----

	// AttrSandboxExitCode is the process exit status of an executed command.
	AttrSandboxExitCode = "sandbox.exit_code"

	// AttrSandboxRunner identifies the runner that executed a command (local,
	// bwrap, seatbelt, windows, ...), so a latency or failure difference between
	// backends is visible. It is the concrete Go type, not a declared name: the
	// attribute answers "who ran it", and a decorator chain would lie about the
	// declared one.
	AttrSandboxRunner = "sandbox.runner"

	// AttrSandboxTimeoutMs is the deadline the caller imposed on one command. A
	// zero value means none and is omitted.
	AttrSandboxTimeoutMs = "sandbox.timeout_ms"

	// ----- Turns -----

	// AttrAgentRound is the 1-based position of a turn within its session. It is
	// correlation, not identity: the renderer already emits "turn" from the run
	// identity, and this one is what a producer that has not attached identity yet can
	// still label its span with.
	AttrAgentRound = "agent.round"

	// AttrAgentTraceLevel is how much of this turn the deployment asked to record.
	// Without it a reader cannot tell "nothing slow happened" from "this deployment
	// recorded nothing".
	AttrAgentTraceLevel = "agent.trace_level"

	// AttrAgentState is the terminal task state a turn reached. Its producer is
	// agent.RunTurn, which is the only layer that sees the whole event stream.
	AttrAgentState = "agent.state"

	// ----- Tools -----

	// AttrToolName identifies the dispatched tool. It is the attribute that makes a
	// "tool.execute" span name groupable instead of one span name per tool.
	AttrToolName = "tool.name"

	// AttrToolCallID identifies one invocation: the id the model issued, so a
	// tool_call event and its tool_result correlate.
	AttrToolCallID = "tool.call_id"

	// AttrToolSource names where a tool came from ("builtin", "mcp", "plugin").
	AttrToolSource = "tool.source"

	// AttrToolLazy marks a tool whose schema was materialised on first use rather
	// than at registration.
	AttrToolLazy = "tool.lazy"

	// AttrCodeStacktrace carries the stack captured where a panic was turned into an
	// error result. The result a caller sees says what panicked, never where, so this
	// is the only evidence a recovered panic leaves behind.
	AttrCodeStacktrace = "code.stacktrace"

	// ----- GenAI semantic conventions -----
	//
	// A fact the OpenTelemetry GenAI conventions define is emitted under gen_ai.*;
	// a fact they do not define stays in llm.* (see the LLM section). Only the
	// emitted spelling is declared, so one fact cannot acquire two names.

	// AttrGenAIOperationName names the operation ("chat", "embeddings", ...).
	// The conventions require it on every inference span.
	AttrGenAIOperationName = "gen_ai.operation.name"

	// AttrGenAIProviderName identifies the provider that served the call
	// ("openai", "anthropic", "azure.ai.openai", ...). Renamed from
	// gen_ai.system in v1.37; do not emit the old key.
	AttrGenAIProviderName = "gen_ai.provider.name"

	// AttrGenAIRequestModel is the model the request asked for.
	AttrGenAIRequestModel = "gen_ai.request.model"

	// AttrGenAIResponseFinishReasons is the finish reason array. It is an
	// array in the conventions because one response may carry several.
	AttrGenAIResponseFinishReasons = "gen_ai.response.finish_reasons"

	// AttrGenAIResponseID is the provider-assigned response object id
	// (OpenAI response.id, Anthropic message.id, a chat completion id).
	AttrGenAIResponseID = "gen_ai.response.id"

	// AttrGenAIResponseModel is the model that actually answered, when the
	// provider reports one (routing may resolve an alias).
	AttrGenAIResponseModel = "gen_ai.response.model"

	// AttrGenAIUsageCacheReadInputTokens is the subset of the prompt served
	// from the provider's prompt cache.
	AttrGenAIUsageCacheReadInputTokens = "gen_ai.usage.cache_read.input_tokens"

	// AttrGenAIUsageInputTokens is the prompt total. It includes cached
	// tokens and every modality, which is exactly the inclusive semantics
	// outcome.Usage.InputTokens already documents.
	AttrGenAIUsageInputTokens = "gen_ai.usage.input_tokens"

	// AttrGenAIUsageOutputTokens is the generated total.
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"

	// ----- GenAI operation values -----
	//
	// The conventions say a custom value MAY be used when none of the well-known
	// ones applies, which is how the three inference seams map without pretending a
	// judgment call is a chat.

	// GenAIOperationChat is a chat-style generation.
	GenAIOperationChat = "chat"
	// GenAIOperationEmbeddings is an embeddings call.
	GenAIOperationEmbeddings = "embeddings"
	// GenAIOperationGenerateContent is generation of a non-text modality (image,
	// audio, video).
	GenAIOperationGenerateContent = "generate_content"
	// GenAIOperationDecide is the judgment seam. The convention has no vocabulary
	// for "answer these typed questions", and calling it chat would describe the
	// wire format instead of the operation.
	GenAIOperationDecide = "decide"

	// ----- Inference (flowcraft's own seams) -----

	// AttrInferenceErrorDetail is the stable, redacted stage label carried by
	// inference.Error.Detail ("stream.finish.missing"). It never holds
	// prompts, credentials or wire payloads, which is why it is safe on a span
	// while the wrapped cause is not.
	AttrInferenceErrorDetail = "inference.error.detail"

	// AttrInferenceOperation names the seam a call belongs to
	// ("generate" / "embed" / "decide"). It is the route/metric dimension,
	// kept distinct from gen_ai.operation.name because a route span must not
	// carry gen_ai.* (a failover would then look like two model calls).
	AttrInferenceOperation = "inference.operation"

	// AttrInferenceShape distinguishes a unary call from a streaming one
	// ("unary" / "stream").
	AttrInferenceShape = "inference.shape"

	// ----- Route (selection, retry, failover) -----

	// AttrRouteAttemptNumber is which attempt within one target this was, so a
	// reader can order the route.attempt events.
	AttrRouteAttemptNumber = "route.attempt.number"

	// AttrRouteAttempts is how many provider attempts one routed call made.
	AttrRouteAttempts = "route.attempts"

	// AttrRouteBackoffMs is the delay a retry waited before its attempt.
	AttrRouteBackoffMs = "route.backoff_ms"

	// AttrRouteExecuted is the candidate that answered. Empty when every
	// candidate failed.
	AttrRouteExecuted = "route.executed"

	// AttrRouteFallbacks is how many times the dispatcher moved to another
	// target.
	AttrRouteFallbacks = "route.fallbacks"

	// AttrRouteOutcome is one attempt's result: "success" or "error".
	AttrRouteOutcome = "route.outcome"

	// AttrRouteRetries is how many of those attempts re-tried the same target.
	AttrRouteRetries = "route.retries"

	// AttrRouteSelected is the first candidate the selector returned.
	AttrRouteSelected = "route.selected"

	// AttrRouteTarget is the candidate a route attempt addressed.
	AttrRouteTarget = "route.target"

	// ----- LLM facts the conventions do not define -----

	// AttrLLMCostMicros stays authoritative: the conventions define no cost
	// key, and the upstream naming is still contested, so squatting on
	// gen_ai.usage.cost.* now would collide when it lands. Micro-units of the
	// configured currency (micro-USD = USD * 1_000_000) keep integer math exact
	// across cumulative budgets; zero when the host has no pricing catalog.
	AttrLLMCostMicros = "llm.cost.micros"

	// AttrLLMRequestID stays authoritative: gen_ai.response.id names the
	// response object, not the request id a provider support ticket asks for.
	AttrLLMRequestID = "llm.request.id"

	// AttrLLMTimeToFirstTokenMs is the delay until the first visible text
	// delta of a stream. The conventions define no such key, and it is not
	// derivable from the span duration: it separates "the model is slow to
	// start" from "the model is verbose", which are different incidents.
	// Reasoning deltas deliberately do not count.
	AttrLLMTimeToFirstTokenMs = "llm.time_to_first_token.ms"

	// ----- Policy, budgets and results -----

	// AttrPolicyCheck names the rule that refused a call. It is the dimension a
	// denial is counted by, so a new value is a deliberate vocabulary change.
	AttrPolicyCheck = "policy.check"

	// AttrPolicyReason is the stable, redacted explanation for a denial. Never a
	// payload, never a path the user did not already supply.
	AttrPolicyReason = "policy.reason"

	// AttrPolicyID is the digest of the session's bind, so a denial names the
	// permission that refused it.
	AttrPolicyID = "policy.id"

	// AttrRateLimitBlockedMs is how long the rate limiter made one call wait: it
	// separates "the tool was slow" from "the fleet was saturated".
	AttrRateLimitBlockedMs = "rate_limit.blocked_ms"

	// AttrResultTruncated marks a result the middleware shortened before the model
	// saw it.
	AttrResultTruncated = "result.truncated"

	// AttrResultDroppedRunes is how much text the limiter removed.
	AttrResultDroppedRunes = "result.truncated.runes"

	// AttrResultSpilledBytes is the size of the payload persisted for the model to
	// read back after truncation.
	AttrResultSpilledBytes = "result.spilled.bytes"

	// ----- Errors -----

	// AttrErrorMessage carries the human-readable error string on log records and
	// span events. It is aligned with the OTel exception.message semantics but keeps
	// the shorter key, because flowcraft logs do not emit the OTel exception-event
	// shape (no exception.type / exception.stacktrace): one canonical key for the
	// text is enough.
	//
	// Producers must use this constant rather than the legacy "error" key, which was
	// used inconsistently — sometimes the message, sometimes a code — so a dashboard
	// could filter on it only by guessing.
	AttrErrorMessage = "error.message"

	// AttrErrorKind is the closed classification of a failure: an
	// inference.ErrorKind on the inference path, or the tool-side
	// vocabulary ("policy_denied", "timeout", "not_found", "tool_error",
	// "panicked"). It is the dimension dashboards group failures by, so a new
	// value is a deliberate vocabulary change rather than a free-form string.
	AttrErrorKind = "error.kind"

	// AttrErrorRetryAfterMs is a server-provided backoff hint, so a consumer
	// can tell "we chose to wait" from "the provider told us to".
	AttrErrorRetryAfterMs = "error.retry_after_ms"
)
