---
layout: default
title: Inference Runtime
---
# Inference Runtime Guide

`core/inference` is the unified, instance-owned runtime for model inference.
Active workloads are `Generate`, `Embed`, and `Transcription`; they are
enumerated once, by `model.Operations()` in `core/inference/model`.

The model declaration vocabulary — identity, descriptor, capabilities,
limits, lifecycle, and the catalog patch language — lives in
`core/inference/model`. `core/inference` re-exports it through deprecated
aliases (`inference.ModelRef` and friends) so existing callers keep
compiling; new code should import `core/inference/model` directly.

## Exact addressing

Every call takes a concrete `ModelRef`:

```go
model := model.ModelRef{
    ID: model.ModelID{
        Provider: "deepseek",
        Name:     "deepseek-flash",
    },
}
```

The runtime never picks or replaces a model. Optional routing is provided by
`core/inference/route`. Route selection consults the providers' declared
model capabilities: targets whose declared output kinds cannot serve the
request intent are skipped, while models with undeclared capabilities are
treated as undeclared (not unsupported) — preflight remains the final
arbiter for those.

## Deployment config

Providers and the assembly are separate resources:

```yaml
resources:
  provider:
    kind: inference.Provider
    impl: openai
    settings:
      id: deepseek
      spec:
        api: responses
        endpoint:
          base_url: https://api.deepseek.com
        wire:
          reasoning_channel: text   # DeepSeek streams plain reasoning text
        catalog: declared
        models:
          - name: deepseek-flash
            kind: generate
            capabilities:
              inputs: [text, image, data, tool_call, tool_result]
              outputs: [text]
      profiles:
        - secrets:
            api_key: ${env:DEEPSEEK_API_KEY}
  infer:
    kind: inference.Assembly
    impl: unified
    deps:
      provider: provider
```

Provider implementations are registered by the application from provider
driver modules:

```go
reg.MustRegister(openai.NewFactory())
reg.MustRegister(inference.Factory{})
```

## Provider lifecycle

Opening a model resolves its credentials and constructs its provider clients.
The default call path (`Assembly.Generate`, `Embed`, `Transcribe`, ...) resolves
the target per call, so nothing is cached behind the caller's back and a
deployment never needs credentials at build time: `Validate` and `InspectModel`
work without them, and a missing credential surfaces on first use.

Callers that want opened drivers to outlive a single call say so explicitly.
`Assembly.Bind(ctx, ref)` opens every operation the model declares and returns
a `Binding`: immutable, safe for concurrent use, owning its drivers until
dropped. Binding again picks up rotated credentials. A binding compiles per
request:

```go
binding, err := assembly.Bind(ctx, model)
prepared, err := binding.PrepareGenerate(ctx, request) // compiles once
response, err := prepared.Execute(ctx)                 // provider I/O only
```

A `Prepared` attempt is what keeps preflight and execution from compiling the
same request twice. `Assembly.Prepare*` returns the same kind of handle for
callers that do not hold a binding, and executing one performs provider I/O
only while still carrying the span, metrics, and usage envelope of a direct
call. Routing uses exactly this: a routed attempt opens and compiles once, then
executes that compilation.

The graph `inference` node is the in-tree example of the binding lifetime: a
node's model is static graph config and the node outlives the turns that run
through it, so it opens each configured model once (keyed by the full model
reference, shared by every node in the graph) and compiles per turn.

The script bridge does the same for script calls: `inference.generate`,
`explain`, `stream`, `embed`, `transcribe`, and `transcribeSession` resolve
their model through one `inference.BindingCache` owned by the bridge, so a
script that keeps addressing the same model reuses its drivers. Both hosts rely
on the same cache type, and both keep working when the model is not one they
have seen before: a miss simply opens it.

That reuse has a staleness window: a credential or client setting rotated after
a model was opened stays in effect until the deployment is rebuilt (or the node
addresses a different profile). Opening is logged with `llm.provider`,
`llm.model`, and `llm.profile`, so "the deployment is still using the previous
key" is diagnosable rather than silent. The node keeps at most 16 bindings;
past that it evicts the least recently used one, so a board-derived model
reference cannot accumulate drivers while the models the graph keeps addressing
stay open.

## Model declarations

Providers expose built-in model catalogs that deployments extend or
override through the provider spec's `models` list. Declarations are
leaf-level patches against the same-named, same-kind built-in entry: a
capability leaf that is written replaces that leaf, while unstated
capability leaves, numeric limits, and driver control facts are inherited —
redeclaring a model to tweak one channel cannot silently revoke the rest,
and removal is explicit (`hosted_web_search: false`, an empty inputs or
outputs list, or reasoning kind `none`). Compatible endpoints configured
through the OpenAI and Anthropic drivers follow the same leaf semantics,
and custom embed dimensions stay tied to the built-in size whitelist.

Capability declarations are promises validated per provider surface: a
model published with reasoning kind `toggle` must compile
`reasoning_enabled=false` on that surface, and one published as `always`
rejects it. Discovery bits such as `hosted_web_search` and
`custom_embed_dimensions` ride on the model descriptor, so hosts can
surface per-model options without driver-specific knowledge.

Declared limits are enforced before any provider work. A `Generate` request
whose `max_output_tokens` exceeds the target model's declared
`max_output_tokens` is rejected at declaration time — the driver is not
opened, and the rejection is transport-safe, so a routed request falls back
to a target that declares room for it instead of failing. The limit is a
promise, not a clamp: the caller's budget is never silently lowered, and an
undeclared limit rejects nothing.

`max_input_tokens` is not enforced: the module has no tokenizer, and a
missing or approximate count would be worse than none. Hosts that own the
conversation history can read the declared input window from
`InspectModel` and enforce it where they already trim context.

## Routing

Optional target selection is an `inference.Router` resource. It consumes
one `inference.Assembly` as its `target` dep and reads the route policy
from its own `settings`:

```yaml
resources:
  router:
    kind: inference.Router
    impl: unified
    deps:
      target: infer
    settings:
      generate:
        - tier: fast
          targets:
            - model: {id: {provider: deepseek, name: deepseek-flash}}
              score: {quality: 0.8, speed: 0.9}
      retry:
        generate:
          max_attempts: 2
          max_total_attempts: 5
          backoff:
            kind: exponential   # fixed | exponential (default)
            initial: 100ms
            max: 2s
            multiplier: 2
            jitter: full        # none | equal | full (default)
          retryable: [rate_limit, timeout, unavailable]
          fallback_on_retry_exhausted: true
      circuit_breaker:
        failure_threshold: 5      # consecutive transient failures; default 5
        recovery_window: 30s      # open-circuit window; default 30s
        half_open_max_probes: 1   # concurrent probes while half-open; default 1
```

The policy has three operation areas — `generate`, `embed`, and
`transcription` — each a list of `tier` pools. A pool is an allowlist of
exact `model` targets plus optional normalized `score` signals
(`quality` / `economy` / `speed` / `reliability`, all in `[0, 1]`).
Scores guide selection only; they never claim a request is executable.

- Route selection is capability-aware: targets whose declared output kinds
  cannot serve the request intent are skipped, while targets with
  undeclared capabilities are treated as undeclared (not unsupported) —
  preflight remains the final arbiter.
- Generate selection honors an optional per-call `model_hint` on the
  request (`provider/name`, or a bare name when exactly one configured
  target carries it). The hint is a preference, not a bypass: a hinted
  target that is absent, unknown, malformed, ambiguous, or whose declared
  output kinds cannot serve the request is skipped, and selection falls
  back to the default policy. When the hinted target is chosen but fails
  at runtime, fallback restarts at the head of the declared order — the
  hinted model is tried first and the rest of the chain keeps its normal
  sequence, never re-attempting the failed hint. The hint matches by
  provider + model name only; credential profiles are not part of the
  hint, so a model configured under several profiles cannot be
  distinguished per call (the deployment's configured profile stays
  authoritative). A hinted target that the assembly reports as retired or
  missing the operation is a selection error on both the hinted and
  default paths (build-time policy validation already rejects such
  targets). The hint is routing metadata — drivers never interpret it —
  and it applies to both unary `Generate` and `GenerateStream`.
- The `retry` section configures per-operation retries (same-target), each
  with `max_attempts`, an optional `max_total_attempts`, a `backoff`
  curve, an explicit `retryable` class list (`rate_limit`, `timeout`,
  `unavailable`), and `fallback_on_retry_exhausted`. A retry section
  requires its operation to have pools.
- `circuit_breaker` opens a per-target circuit after consecutive transient
  failures, probes while half-open, and skips attempts while open.
- At build time every configured target is validated against the assembly:
  it must exist, not be retired, and expose the operation. A graph engine
  wires the router through its `router` dep when inference nodes omit an
  explicit `model`.

## Request metadata

`GenerateRequest.RequestMetadata` is an opaque `map[string]string` carried
with every call. Core never interprets its keys; callers and hosts decide
the vocabulary (conversation identifiers, turn identifiers, installation
metadata, ...). Graph inference nodes expose the same field as
`request_metadata` node config, and script/direct callers can set it on the
canonical request.

Drivers forward the bag only when their deployment enables it, because each
provider API has a different legal shape. Provider specs accept:

```yaml
settings:
  spec:
    request_metadata:
      envelope: metadata          # any non-empty top-level field name; empty disables
```

OpenAI-compatible drivers map `metadata` onto the native OpenAI metadata
object; `client_metadata` is emitted as a passthrough object for gateways
that speak the Codex convention. An empty configuration never sends
anything, and core keys are forwarded verbatim.

`request_metadata` forwarding is implemented by the OpenAI driver, which
covers OpenAI, Azure, DeepSeek, Kimi and any compatible endpoint configured
through its `endpoint` block. Anthropic, MiniMax, and Bytedance are the
current exceptions: their official Messages/Ark surfaces do not model
arbitrary request metadata and the drivers deliberately keep their native
transport paths, so canonical metadata is not forwarded until those
SDKs/providers add a native channel.

Because `RequestMetadata` is part of the compile ledger, drivers that cannot
forward it report a `dropped` decision instead of silently ignoring it.
Drivers that forward it report `native` when their deployment enables an
envelope. The envelope is an arbitrary non-empty string naming the top-level
body field; providers that type `metadata` natively lower that name through
their SDK types, while other names ride as passthrough JSON fields.

### Component notes

Decisions are field-level: every active canonical field carries exactly one
terminal disposition. Some fields aggregate several content parts under one
path — the `*` in `generate.context.*.content.parts.tool_result` spans every
message — so a single disposition cannot say "the text arrived but the image
did not". Such a decision may carry `components` notes: one entry per part,
in encounter order, each naming the content kind, its position, and its own
disposition. The notes are populated only when the field lost at least one
part, and they must fold onto the field's disposition, so a field can never
read `native` while one of its components was dropped.

The OpenAI driver uses them for multimodal tool results: text that rides
along is `native`, an image the wire cannot carry (a Chat Completions tool
message, a model without image input, an unmaterialized stream source) is
`dropped` at its own position with a reason, and the model receives an
in-place `[omitted tool output: ...]` placeholder so the surrounding text
keeps its meaning.

## Streaming

`GenerateStream` returns a stream of deltas plus the final result. Streaming
is provider-neutral; each driver adapts its native protocol.

## Media streams

Live media input is a first-class transport, not a new DTO: a stream is a
sequence of ordinary `message.Part`s. The media layer owns the generic
pull contract (`media.Stream[T]` / `media.Pipe[T]`); `message.Stream` is
that contract instantiated over `Part`, and `message.NewPartPipe` builds a
bounded pipe whose `Send` blocks when the buffer is full — that is the
backpressure contract. `Interrupt` aborts a stream (barge-in, error), while
`Close` ends it normally; after `Interrupt`, `Read` returns
`context.Canceled` even if buffered parts remain.

An `AudioSource` or `VideoSource` can carry a live stream via
`message.NewAudioStream` / `message.NewVideoStream` (source kind
`stream`). Stream sources are valid only while a message is in flight:

- Unary `Generate` rejects stream sources in both context and input — a
  model call receives complete parts, never a live handle.
- Stream sources cannot be serialized, so they never cross the wire as a
  value.
- When a message becomes history (the run commits), the runtime
  materializes each stream source into the existing inline-byte part:
  audio chunks become one `AudioPart` with inline bytes, video chunks one
  `VideoPart`. This mirrors how `GenerateStream` accumulates deltas and
  materializes them at the terminal result.

## Transcription

Speech recognition is a first-class workload with two execution shapes:

- `Transcribe` recognizes one complete audio source (`media.AudioSource`)
  and returns the transcript, with optional segments and timestamps.
- `TranscribeSession` opens a duplex session: the caller negotiates
  `input_format` at open, feeds `media.AudioChunk`s through `Send`, and
  drains partial/final transcript events through `Next`. `Interrupt`
  terminates a session abnormally (barge-in); draining to `io.EOF` and
  calling `Result` yields the final transcript.

Both shapes address the same `ModelRef` and share the Transcription route
pools. Drivers that only serve one shape leave the other opener nil and the
assembly reports `UnsupportedOperation` for it.

Sessions may emit multiple `Final` events for continuous recognition. A
provider session can expose the optional `TranscriptionSessionFinisher`
capability: callers that have no more audio call `FinishInput`, then drain
`Next` to `io.EOF` and read `Result`. `TranscribeStream` performs that
end-of-input handshake automatically after the source stream ends; the
script bridge exposes it as the session handle's `finish()`.

Live input rides the part-stream transport: `FeedTranscription` pumps a
`message.Stream[Part]` into an open session (audio parts become chunks with
monotonic sequence; EOF ends feeding; a stream failure interrupts the
session), and `TranscribeStream` is the one-shot open + feed + drain +
result form. Unary `Transcribe` rejects stream sources — whole-file
recognition takes complete audio, live audio goes through a session.

See [graph.md](graph.md) for the inference node.
