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

See [graph.md](graph.md) for the inference node.
