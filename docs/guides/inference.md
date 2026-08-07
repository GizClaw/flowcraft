---
layout: default
title: Inference Runtime
---
# Inference Runtime Guide

`sdk/inference` is FlowCraft's unified, instance-owned runtime for model
inference. Every workload is one of four root operations:

| Operation       | Shape                  | Covers                                                                                |
| --------------- | ---------------------- | ------------------------------------------------------------------------------------- |
| `Generate`      | unary or finite stream | text, tools, JSON-schema output, image generation, speech synthesis, video generation |
| `Embed`         | unary                  | ordered items → ordered vectors (text and multimodal)                                 |
| `Transcription` | unary or session       | speech recognition, long-lived ASR sessions                                           |
| `Realtime`      | bidirectional session  | full-duplex voice dialogue                                                            |

Chat, image generation, and speech synthesis are **not** separate APIs —
they are `Generate` requests with different intents, and share request,
response, streaming, routing, and usage contracts.

Two rules make the system predictable:

- **Exact addressing.** Every call takes one concrete `ModelRef`
  (provider + model + credential profile). The runtime never picks or
  replaces a model for you. Optional routing lives in
  `sdk/inference/route`, composed above the runtime.
- **Compile authority.** The provider compiler is the only judge of
  whether a request is executable. Every active field is either `Native`
  or a structured rejection (`UnsupportedFeature` / `InvalidExtension`) —
  never a silent drop or semantic downgrade. `Explain*` runs the same
  compile without provider I/O.

## Concepts

### ModelRef

```go
model := inference.ModelRef{
    ID:      inference.ModelID{Provider: "openai", Name: "gpt-5.4"},
    Profile: "", // "" selects the provider's default credential profile
}
```

Providers are deployment instances with an ID you choose (`"openai"`,
`"qwen-eu"`, ...). A provider may carry several **profiles** — named
credentials scoped to the operations they may execute (e.g. a
speech-only key). The model must exist in the provider's catalog (or its
custom `spec.models`).

### Generate request model

```go
type GenerateRequest struct {
    Context []Message     // already-observed history; cannot carry Intent
    Input   GenerateInput // the one input being executed; owns Intent
}
```

Intent declares required output on the current input only. It composes
freely — `Text` (+ tools, sampling, reasoning), `Image`, `Audio`,
`Video` — with at least one output modality required. After execution,
`input.Message()` converts the input into future history, discarding
intent by construction.

### Extensions

Provider-specific settings ride on requests as typed values addressed by
provider ID:

```go
request.Extensions = inference.Extensions{
    qwen.GenerateOptions{ThinkingBudget: int64Ptr(4096)},
    kimi.GenerateOptions{PromptCacheKey: "session-42"},
}
```

A request may carry several providers' extensions at once. On each route
attempt the pipeline strips extensions addressed elsewhere
(`Extensions.ForProvider`), so fallback across providers applies only
the executing provider's settings; the rest are inert. An extension
addressed to the executing provider that is unknown, meant for another
operation, or colliding with a canonical field is a structured
rejection.

## Deployment configuration

Deployments are described by a versioned `config.Document`. Documents
contain **secret references only** — never plaintext credentials — and
the envelope validator rejects credential-looking keys anywhere in
`spec`. YAML is the on-disk format (`sdk/inference/config`);
`config.Parse` reads both YAML and JSON through the shared
`sdk/config` protocol.

```yaml
# inference.yaml
version: v1
providers:
  - id: openai
    driver: openai
    profiles:
      - secrets: # default profile (no id)
          api_key: { resolver: env, key: OPENAI_API_KEY }
  - id: bytedance
    driver: bytedance
    profiles:
      - secrets:
          api_key: { resolver: env, key: ARK_API_KEY }
      - id: speech
        operations: [transcription, realtime]
        secrets:
          speech_api_key: { resolver: env, key: DOUBAO_SPEECH_API_KEY }
        spec:
          app_id: "123456"
  - id: qwen
    driver: qwen
    spec:
      models: # custom models merge with the built-in catalog
        - name: qwen3.7-max-2026-08
          kind: generate
          reasoning: true
    profiles:
      - secrets:
          api_key: { resolver: env, key: DASHSCOPE_API_KEY }
route: # optional; omit to address exact models yourself
  generate:
    - tier: primary
      targets:
        - model: { id: { provider: openai, name: gpt-5.4 } }
        - model: { id: { provider: qwen, name: qwen3.7-max } }
```

Resolver names (`env`, `file`, ...) reference the resolver catalog you
hand to the builder; `spec` fields are decoded strictly by the owning
provider package — see its `spec.go` / `doc.go` for the schema and
secret names.

## Building the runtime

```go
import (
    "os"

    "github.com/GizClaw/flowcraft/sdk/inference/config"
    "github.com/GizClaw/flowcraft/sdk/inference/config/env"
    "github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
    "github.com/GizClaw/flowcraft/sdkx/inference/openai"
    "github.com/GizClaw/flowcraft/sdkx/inference/qwen"
)

data, err := os.ReadFile("inference.yaml")
if err != nil { /* ... */ }
document, err := config.Parse(data)
if err != nil { /* ... */ }

builder, err := config.NewBuilder(
    map[string]config.Factory{ // driver name → provider factory
        "openai":    openai.Factory(),
        "bytedance": bytedance.Factory(),
        "qwen":      qwen.Factory(),
    },
    map[string]config.SecretResolver{ // resolver name → credential source
        "env": env.New(),
    },
)
if err != nil { /* ... */ }

assembly, err := builder.NewAssembly(ctx, document)
if err != nil { /* ... */ }

runtime := assembly.Runtime // *inference.Runtime
router  := assembly.Router  // *route.Router, nil without a route section
```

`Builder` validates the document, resolves every `SecretRef`, lets each
factory assemble its provider definition, and fails fast on malformed
output or route targets pointing at unknown models.

## Generate

### Unary

In the snippets below, `message` is
`github.com/GizClaw/flowcraft/sdk/message` — request history and content
use the shared message DTOs, not inference-specific types.

{% raw %}
```go
model := inference.ModelRef{ID: inference.ModelID{Provider: "openai", Name: "gpt-5.4"}}

resp, err := runtime.Generate(ctx, model, inference.GenerateRequest{
    Context: []message.Message{{
        Role: message.RoleSystem,
        Content: message.Content{Parts: []message.Part{
            message.TextPart{Text: "Answer concisely."},
        }},
    }},
    Input: inference.GenerateInput{
        Role: inference.InputRoleUser,
        Content: inference.InputContent{
            Content: message.Content{Parts: []message.Part{
                message.TextPart{Text: "Give me three names for a Go linter."},
            }},
            Intent: inference.Intent{Text: &inference.TextIntent{
                MaxOutputTokens: intPtr(256),
                Temperature:     floatPtr(0.4),
            }},
        },
    },
})
// resp.Message.Content.Parts, resp.FinishReason, resp.Usage, resp.Metadata
```
{% endraw %}

`intPtr` / `floatPtr` / `int64Ptr` in these snippets are local
address-taking helpers (`func intPtr(v int) *int { return &v }`), not
part of the SDK — optional canonical fields are pointers so the field
ledger can tell "unset" from "explicitly zero".

### Streaming

```go
stream, err := runtime.GenerateStream(ctx, model, request)
if err != nil { /* ... */ }
defer stream.Close()

for {
    event, err := stream.Next(ctx)
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        return err // typed *inference.Error — switch on its Kind
    }
    switch delta := event.Delta.(type) {
    case inference.TextPartDelta:
        fmt.Print(delta.Text)
    case inference.ReasoningDelta:
        // thinking trace fragment
    case inference.ToolCallDelta:
        // tool call assembled at its PartIndex
    }
    // event.Usage is a cumulative snapshot when present
}
final, err := stream.Result() // complete GenerateResponse after EOF
```

### Multi-turn and tool calling

History is explicit: append the executed input (`input.Message()`) and
the response message, then issue the next request. Tool continuation is
a `tool`-role input carrying `ToolResultPart` values:

```go
context = append(context,
    input.Message(),  // the user input just executed
    resp.Message,     // assistant reply (may contain ToolCallPart)
)

// After running the tool locally:
next := inference.GenerateRequest{
    Context: context,
    Input: inference.GenerateInput{
        Role: inference.InputRoleTool,
        Content: inference.InputContent{
            Content: message.Content{Parts: []message.Part{
                message.ToolResultPart{Result: message.Result{
                    CallID:  call.ID,
                    Content: `{"temperature": "21C"}`,
                }},
            }},
            Intent: inference.Intent{Text: &inference.TextIntent{Tools: tools}},
        },
    },
}
```

### Media intents

Image, audio (speech/music), and video generation are `Generate`
requests on models whose catalog entry supports the modality:

```go
// Speech synthesis (bytedance TTS, MiniMax speech, openai gpt-4o-mini-tts):
Intent: inference.Intent{Audio: &inference.AudioIntent{
    Voice:  media.VoiceSpec{ID: "zh_female_qingxin"},
    Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
}}

// Image generation (openai gpt-image-1, MiniMax image-01, bytedance seedream):
Intent: inference.Intent{Image: &inference.ImageIntent{
    Count: intPtr(1),
    Size:  &media.ImageSize{Width: 1024, Height: 1024},
}}

// Video generation (MiniMax Hailuo, bytedance seedance):
Intent: inference.Intent{Video: &inference.VideoIntent{
    Resolution:     "720p",
    DurationMillis: int64Ptr(6000),
}}
```

Response parts arrive as `ImagePart` / `AudioPart` / `VideoPart` (bytes
or URL, per model). If a field cannot be honored natively — say a size
the model lacks — the compiler rejects it with the field named, instead
of silently approximating.

### Embed

```go
resp, err := runtime.Embed(ctx, embedModel, inference.EmbedRequest{
    Items: []inference.EmbedItem{
        {Content: message.Content{Parts: []message.Part{
            message.TextPart{Text: "first document"},
        }}},
        {Content: message.Content{Parts: []message.Part{
            message.TextPart{Text: "second document"},
        }}},
    },
    Dimensions: intPtr(1024), // rejected unless the model supports it
})
// resp.Embeddings is ordered 1:1 with Items; resp.Usage counts tokens
```

Multimodal embedding models (e.g. `qwen3-vl-embedding`) accept image and
mixed parts in `EmbedItem.Content`.

## Transcription and Realtime

```go
// Finite recognition:
resp, err := runtime.Transcribe(ctx, asrModel, inference.TranscriptionRequest{ /* audio */ })

// Long-lived ASR session:
session, err := runtime.OpenTranscription(ctx, asrModel, sessionConfig)

// Full-duplex dialogue (bytedance today):
session, err := runtime.OpenRealtime(ctx, realtimeModel, inference.RealtimeConfig{
    Instructions: "You are a voice assistant.",
    Modalities:   []inference.Modality{inference.ModalityAudio, inference.ModalityText},
    OutputAudioFormat: &media.AudioFormat{Encoding: media.AudioEncodingPCM16, SampleRateHz: 24000},
    Voice:            &media.VoiceSpec{ID: "zh_female_qingxin"},
})
defer session.Close()
// session.Send(ctx, inference.RealtimeAudioInput{Chunk: chunk})
// event, err := session.Next(ctx) → RealtimeAudioDeltaEvent / RealtimeToolCallEvent / ...
// session.CancelResponse(ctx) to barge in
```

`Send` and one `Next` may run concurrently. Provider bookkeeping events
(session lifecycle, buffer commits) never surface as canonical events.

## Routing and fallback

With a `route` section in the document, `assembly.Router` drives
selection and fallback across the declared target pools, in declared
order:

```go
resp, trace, err := router.Generate(ctx, request) // no ModelRef — the policy selects
for _, attempt := range trace.Attempts {
    log.Printf("attempt %s phase=%s outcome=%s kind=%s",
        attempt.Target.ID.Name, attempt.Phase, attempt.Outcome, attempt.ErrorKind)
}
// trace.Executed is the model that actually produced the response
```

Fallback only ever substitutes another exact `ModelRef`; it never
changes request semantics. A failed compile (preflight) or a failed
execution both advance to the next policy target; a request that fails
canonical validation never enters selection at all. Custom selection
logic (score-aware, request-aware) builds `route.New(runtime,
selectors)` directly.

## Retry, backoff, and circuit breaker

`route.Router` can retry a transient provider failure on the same target
before falling back. Retries are conservative by default: only
`ProviderFailure` classified as rate limit, timeout, or unavailable is
retried, never after observable output, and never for validation,
compile rejections, policy denials, or context cancellation. Streams and
sessions only retry before they open; once a stream or session is
returned, the Router never transparently reopens it.

The route policy section declares retries and the per-target circuit
breaker. The DTO is JSON (the config entry point still accepts YAML and
converts it to JSON):

```json
{
  "route": {
    "generate": [
      {"tier": "primary", "targets": [{"model": {"id": {"provider": "openai", "name": "gpt"}}}]}
    ],
    "retry": {
      "generate": {
        "max_attempts": 3,
        "max_total_attempts": 8,
        "backoff": {"kind": "exponential", "initial": "100ms", "max": "2s", "jitter": "full"},
        "retryable": ["rate_limit", "timeout", "unavailable"],
        "fallback_on_retry_exhausted": false
      }
    },
    "circuit_breaker": {
      "failure_threshold": 5,
      "recovery_window": "30s",
      "half_open_max_probes": 1
    }
  }
}
```

`fallback_on_retry_exhausted` is off by default: a transient provider
failure never moves to another target unless the deployment opts in.
Circuit-breaker state is per `Operation + ModelRef` and lives on the
`Router` instance; open targets are skipped without wire attempts, and a
half-open circuit admits one probe at a time.

`http_retries` on any provider spec means total wire attempts including
the first. On httpkit-backed providers it sets the transport retry
budget; on openai / anthropic / azure / deepseek it maps to the vendor
SDK's max-retries option. `0` disables provider-level retries so the
Router owns the whole budget.

`max_total_attempts` caps logical attempts across all targets for one
request (0 or absent means no cap). Circuit state can be cleared at
runtime with `Router.ResetCircuitBreaker()`.

Programmatic callers build the same behavior with
`route.WithRetryPolicies` and `route.WithCircuitBreaker`.

## Hot reload

There is no built-in `config.Reloader` in the current stack. `Builder` and
`Assembly` are immutable, so hot reload means building a fresh `Assembly`
from a new `Document` and swapping it atomically in application code while
the old runtime finishes its in-flight calls. A failed parse or build never
affects the previously serving runtime.

## Debugging: Explain

Every operation has an `Explain*` variant that resolves the model and
runs the full compile **without provider I/O** — the exact wire plan and
field-by-field decisions:

```go
explanation, err := runtime.ExplainGenerate(ctx, model, request)
for _, decision := range explanation.Decisions {
    // decision.Field, decision.Disposition (Native | Rejected), decision.Reason
}
```

Use it in tests and tooling to answer "would this request run, and what
happens to each field?" without spending tokens.

## Telemetry

Instrumentation is **always on** and needs no configuration: with no
OTel SDK installed the global no-op providers make every hook a few map
lookups. Point an OTel SDK at the process and spans, metrics, and
failure logs appear — for direct `Runtime` callers and for routed
callers alike (each routing attempt delegates to a `Runtime` call, so
attempt spans nest under the route span).

Every operation emits one span at the `Runtime` funnel:

- `inference.generate` / `inference.embed` / `inference.transcribe` /
  `inference.realtime` (streaming adds a `.stream` suffix)
- Attributes: `inference.operation`, `llm.provider`, `llm.model`,
  `inference.profile` — the `llm.*` keys match the rest of the
  telemetry surface so dashboards can join across packages
- Usage rides the span on success: `llm.tokens.input` / `.output` /
  `.total` (plus `llm.tokens.input.cached` when the provider reports
  prompt-cache reads); transcription adds `inference.audio.duration_ms`
- Failures set the span status to Error, stamp `inference.error_kind`
  (the `ErrorKind` taxonomy above), and log a warning through
  `sdk/telemetry`

Metrics (meter `flowcraft/inference`):

| Metric                           | Labels                                 | Meaning                              |
| -------------------------------- | -------------------------------------- | ------------------------------------ |
| `executions.total`               | operation, provider, model, status     | calls by outcome                     |
| `duration.seconds`               | operation, provider, model             | unary latency / stream time-to-close |
| `errors.total`                   | operation, provider, model, error_kind | failures by `ErrorKind`              |
| `tokens.input` / `tokens.output` | provider, model                        | generate spend                       |
| `tokens.input.cached`            | provider, model                        | prompt-cache hits                    |

A streaming span stays open for the life of the stream — it closes on
`io.EOF`, on a terminal error, or on `Close`, and records the last
cumulative usage snapshot. A stream abandoned without EOF or `Close`
leaks its span (the standard streaming-API trade-off), so always close
streams.

The `Router` adds one route-level span per logical request,
`inference.route.<operation>` (meter `flowcraft/inference.route`):
each attempt becomes a span event (`target`, `phase`, `trigger`,
`outcome`, `error_kind`), and attributes record the journey —
`route.selected.*` vs `route.executed.*`, `route.attempts`,
`route.fallbacks` — alongside `executions.total` and `fallbacks.total`
counters. This answers "did we fall back, how often, and why" without
spelunking per-attempt spans.

`Explain*` methods perform no provider I/O and stay silent.

## Errors

Runtime errors are typed (`*inference.Error`) with a stable
`ErrorKind`: `invalid_request`, `unsupported_operation`,
`unsupported_feature`, `invalid_extension`, `unknown_provider` /
`unknown_model` / `unknown_profile`, `policy_denied`,
`operation_interrupted`, `compiler_contract_violation`,
`provider_failure`, and `invalid_provider_response`. Routing adds its
own kinds (`no_route`, `selection_failed`, `fallback_failed`,
`fallback_limit_exceeded`, ...). Provider HTTP/SDK failures are
classified into the same taxonomy (a 429 surfaces as
`provider_failure` with a rate-limit cause), so callers switch on kind,
not on message text.

## Provider inventory

All providers ship in `sdkx/inference/<name>` with per-package `doc.go`
covering protocol quirks, catalog, spec schema, and extensions.

| Driver      |            Generate            |        Embed         |    Transcription    | Realtime | Secret(s)                                                |
| ----------- | :----------------------------: | :------------------: | :-----------------: | :------: | -------------------------------------------------------- |
| `anthropic` |               ✓                |                      |                     |          | `api_key`                                                |
| `azure`     |               ✓                |          ✓           |          ✓          |          | `api_key`                                                |
| `bytedance` | ✓ (+image/audio/video intents) |          ✓           | ✓ (unary + session) |    ✓     | `api_key`, `speech_api_key`, `access_key` / `secret_key` |
| `deepseek`  |               ✓                |                      |                     |          | `api_key`                                                |
| `kimi`      |               ✓                |                      |                     |          | `api_key`                                                |
| `minimax`   | ✓ (+image/audio/video intents) |                      |                     |          | `api_key`                                                |
| `openai`    |    ✓ (+image/audio intents)    |          ✓           |          ✓          |          | `api_key`                                                |
| `qwen`      |               ✓                | ✓ (incl. multimodal) |                     |          | `api_key`                                                |

Each factory merges your `spec.models` over its built-in catalog, so new
provider releases are usable the day they ship by declaring them
yourself (name, kind, capability flags).

## Testing your application

`Runtime` is assembled from plain `inference.ProviderDefinition` values
— tests can register a fake provider without any network by binding
handlers with `inference.BindGenerate` / `BindEmbed` and returning
canned responses. Provider packages themselves are held to the shared
black-box contract suites in `sdk/inference/inferencetest` (generate
unary/stream, compile parity, rejection accounting, realtime sessions).

## Further reading

- Package contracts: `sdk/inference/doc.go`,
  `sdk/inference/config/doc.go`, `sdk/inference/route/doc.go`
- Per-provider guides: `sdkx/inference/<provider>/doc.go`
- Provider inventory and config schema:
  `sdk/inference/config/doc.go`, each `sdkx/inference/<provider>/spec.go`.
