---
layout: default
title: Inference Runtime
---
# Inference Runtime Guide

`core/inference` is the unified, instance-owned runtime for model inference.
Active workloads are `Generate`, `Embed`, and `Transcription`. `Realtime`
is reserved in the operation enum and field ledger but has no request or
session surface yet — it lands in a later milestone.

## Exact addressing

Every call takes a concrete `ModelRef`:

```go
model := inference.ModelRef{
    ID: inference.ModelID{
        Provider: "deepseek",
        Name:     "deepseek-v4-flash",
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
    impl: deepseek
    settings:
      id: deepseek
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
reg.MustRegister(deepseek.NewFactory())
reg.MustRegister(inference.Factory{})
```

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
            - model: {id: {provider: deepseek, name: deepseek-v4-flash}}
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
