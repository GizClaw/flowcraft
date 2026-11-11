package telemetry

// GenAI semantic-convention keys.
//
// A fact the OpenTelemetry GenAI conventions define is emitted under gen_ai.*;
// a fact they do not define stays under llm.* (see attrs.go). The split exists
// because emitting both forms would give one fact two sources of truth, and
// because the conventions deliberately do not cover cost or a provider's
// request id.
//
// The keys here are string constants rather than imports: the conventions moved
// out of semantic-conventions into their own module in v1.44 and that module has
// not cut a release, so there is nothing stable to depend on. SemconvGenAIVersion
// records the last numbered revision this file was checked against; bumping it is
// a deliberate, reviewable change, and TestAttrsHaveOneHome keeps the two
// namespaces from drifting apart.
//
// Builders that only know the conventions (Grafana, Langfuse, Phoenix, ...) group
// LLM traffic by these keys, so an LLM span is expected to carry them even though
// flowcraft's own dashboards may read llm.* for the keys that have no equivalent.

// SemconvGenAIVersion is the GenAI semantic-convention line these keys were
// aligned with. The registry lives in open-telemetry/semantic-conventions-genai,
// which has never published a release; every key there is still development
// stability. Record the version here so a rename is a one-file change.
const SemconvGenAIVersion = "1.37.0"

const (
	// AttrGenAIOperationName names the operation ("chat", "embeddings", ...).
	// The conventions require it on every inference span.
	AttrGenAIOperationName = "gen_ai.operation.name"

	// AttrGenAIProviderName identifies the provider that served the call
	// ("openai", "anthropic", "azure.ai.openai", ...). Renamed from
	// gen_ai.system in v1.37; do not emit the old key.
	AttrGenAIProviderName = "gen_ai.provider.name"

	// AttrGenAIRequestModel is the model the request asked for.
	AttrGenAIRequestModel = "gen_ai.request.model"

	// AttrGenAIResponseModel is the model that actually answered, when the
	// provider reports one (routing may resolve an alias).
	AttrGenAIResponseModel = "gen_ai.response.model"

	// AttrGenAIResponseID is the provider-assigned response object id
	// (OpenAI response.id, Anthropic message.id, a chat completion id).
	AttrGenAIResponseID = "gen_ai.response.id"

	// AttrGenAIResponseFinishReasons is the finish reason array. It is an
	// array in the conventions because one response may carry several.
	AttrGenAIResponseFinishReasons = "gen_ai.response.finish_reasons"

	// AttrGenAIUsageInputTokens is the prompt total. It includes cached
	// tokens and every modality, which is exactly the inclusive semantics
	// usage.Usage.InputTokens already documents.
	AttrGenAIUsageInputTokens = "gen_ai.usage.input_tokens"

	// AttrGenAIUsageOutputTokens is the generated total.
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"

	// AttrGenAIUsageCacheReadInputTokens is the subset of the prompt served
	// from the provider's prompt cache.
	AttrGenAIUsageCacheReadInputTokens = "gen_ai.usage.cache_read.input_tokens"
)

// Operation values for AttrGenAIOperationName. The conventions say a custom
// value MAY be used when none of the well-known ones applies, which is how
// flowcraft's three seams map without pretending a judgment call is a chat.
const (
	// GenAIOperationChat is a chat-style generation.
	GenAIOperationChat = "chat"
	// GenAIOperationEmbeddings is an embeddings call.
	GenAIOperationEmbeddings = "embeddings"
	// GenAIOperationGenerateContent is generation of a non-text modality
	// (image, audio, video).
	GenAIOperationGenerateContent = "generate_content"
	// GenAIOperationDecide is flowcraft's judgment seam. The convention has
	// no vocabulary for "answer these typed questions", and calling it chat
	// would describe the wire format instead of the operation.
	GenAIOperationDecide = "decide"
)

// llmFactHome records where one LLM-related fact is emitted. Exactly one
// destination is valid per fact:
//
//   - GenAI set: the conventions define the key; emit it and not the llm.* one.
//   - Note only: there is no convention key, the llm.* key stays authoritative.
//
// The table is the single source of truth TestAttrsHaveOneHome enforces, so a
// new llm.* or gen_ai.* key cannot be added without deciding which namespace owns
// it.
type llmFactHome struct {
	// GenAI is the gen_ai.* key that replaces the llm.* key. Empty when the
	// llm.* key stays authoritative.
	GenAI string
	// Note explains the choice; required when GenAI is empty.
	Note string
}

// llmFacts maps every llm.* attribute to its home.
var llmFacts = map[string]llmFactHome{
	AttrLLMProvider: {
		GenAI: AttrGenAIProviderName,
	},
	AttrLLMModel: {
		GenAI: AttrGenAIRequestModel,
		Note:  "the response Metadata carries the resolved model, emitted as " + AttrGenAIResponseModel,
	},
	AttrLLMInputTokens: {
		GenAI: AttrGenAIUsageInputTokens,
	},
	AttrLLMOutputTokens: {
		GenAI: AttrGenAIUsageOutputTokens,
	},
	AttrLLMCachedInputTokens: {
		GenAI: AttrGenAIUsageCacheReadInputTokens,
	},
	AttrLLMResponseID: {
		GenAI: AttrGenAIResponseID,
	},
	// No convention key: the conventions say nothing about cost, and the
	// naming is still contested upstream (LiteLLM, OpenInference and
	// OpenRouter each invented a different gen_ai.cost.*/llm.cost.* shape).
	// Squatting on gen_ai.usage.cost.* now would collide when it lands.
	AttrLLMCostMicros: {
		Note: "semconv does not define cost; llm.cost.micros stays authoritative",
	},
	// No convention key: gen_ai.response.id is the response object, not the
	// request id a provider support ticket asks for.
	AttrLLMRequestID: {
		Note: "semconv has no provider request id; llm.request.id stays authoritative",
	},
	// Derivable, so emitting it would be a third way to say the same thing.
	AttrLLMTotalTokens: {
		Note: "equals input+output; never emitted as an attribute",
	},
	// The conventions define no time-to-first-token, and it is not derivable
	// from the span duration.
	AttrLLMTimeToFirstTokenMs: {
		Note: "semconv has no time-to-first-token; llm.time_to_first_token.ms stays authoritative",
	},
	AttrLLMLatencyMs: {
		Note: "the span duration already carries it; usage.Usage.LatencyMs covers non-span consumers",
	},
}

// genAIWithoutPredecessor lists gen_ai.* keys that have no llm.* counterpart
// because the fact did not exist before the conventions: the operation name, the
// resolved response model, and the finish reasons. They are part of the
// conventions-only vocabulary.
var genAIWithoutPredecessor = map[string]struct{}{
	AttrGenAIOperationName:         {},
	AttrGenAIResponseModel:         {},
	AttrGenAIResponseFinishReasons: {},
}
