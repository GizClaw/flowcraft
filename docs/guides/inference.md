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

Live input rides the part-stream transport: `FeedTranscription` pumps a
`message.Stream[Part]` into an open session (audio parts become chunks with
monotonic sequence; EOF ends feeding; a stream failure interrupts the
session), and `TranscribeStream` is the one-shot open + feed + drain +
result form. Unary `Transcribe` rejects stream sources — whole-file
recognition takes complete audio, live audio goes through a session.

See [graph.md](graph.md) for the inference node.
