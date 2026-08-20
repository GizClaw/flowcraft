---
layout: default
title: Inference Runtime
---
# Inference Runtime Guide

`core/inference` is the unified, instance-owned runtime for model inference.
All workloads are one of `Generate`, `Embed`, `Transcription`, or `Realtime`.

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
`core/inference/route`.

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

Provider implementations live in `driver/<name>` modules. The application
registers the desired provider factories:

```go
reg.MustRegister(deepseek.NewFactory())
reg.MustRegister(inference.Factory{})
```

## Custom models and capability declaration

Drivers ship built-in model catalogs. To add a model outside the catalog — or
override a catalog entry entirely — declare it in the provider `spec.models`.
`kind` selects the compiler family (`generate`, `image`, `tts`, `embed`) and
`capabilities` declares what the model accepts and produces:

```yaml
resources:
  provider:
    kind: inference.Provider
    impl: openai
    settings:
      id: openai
      profiles:
        - id: default
          secrets:
            api_key: ${env:OPENAI_API_KEY}
      spec:
        models:
          - name: gpt-5.6-x
            kind: generate
            capabilities:
              inputs: [text, image, data, tool_call, tool_result]
              outputs: [text]
              hosted_web_search: true
              reasoning: always
          - name: my-image
            kind: image
            capabilities:
              inputs: [text]
              outputs: [image]
          - name: my-tts
            kind: tts
            capabilities:
              inputs: [text]
              outputs: [audio]
          - name: my-embed
            kind: embed
            dimensions: true
```

`kind` is an implementation discriminator — which compiler to bind — not a
capability declaration. Each family contract requires declared outputs to
match it, so kind and capabilities cannot drift:

| kind       | compiler surface | required output |
|------------|------------------|-----------------|
| `generate` | chat/responses   | `text`          |
| `image`    | images API       | `image`         |
| `tts`      | speech API       | `audio`         |
| `embed`    | embeddings API   | none (outputs must be empty) |

`capabilities.inputs` and `capabilities.outputs` use the canonical content
kinds: `text`, `image`, `audio`, `video`, `file`, `data`, `tool_call`,
`tool_result`, `reasoning`. Outputs are restricted to the four output
modalities (`text`, `image`, `audio`, `video`). `hosted_web_search` marks
provider-side web search tool support. `reasoning` declares the reasoning
control capability: `always` (reasoning on, effort adjustable, cannot be
disabled), `toggle` (switchable), or omitted for no reasoning channel.

A model that declares no capabilities is treated as undeclared rather than
unsupported: routing keeps order-based selection and preflight remains the
final arbiter. Models with declared outputs participate in capability-aware
routing — an `ImageIntent` request selects a target whose `outputs` include
`image` instead of failing preflight on a text model. A minimal text-only
model only needs its output declared:

```yaml
          - name: my-plain
            kind: generate
            capabilities:
              outputs: [text]
```

The provider spec is strictly decoded, so the removed `vision`,
`web_search`, and `reasoning` fields must move into `capabilities` — unknown
keys are rejected at build time.

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
