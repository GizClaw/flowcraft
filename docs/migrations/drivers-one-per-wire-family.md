---
layout: default
title: Migrating to one driver per wire family
---
# Migrating to one driver per wire family

The provider drivers were consolidated: one configurable driver serves a whole
wire family, and a deployment points it at the endpoint it wants. This page is
the migration path for deployments and Go callers that named one of the driver
modules that are now gone.

## What moved

| Removed module | Use instead | How the new driver is pointed |
| --- | --- | --- |
| `driver/azure` | `driver/openai` | `spec.endpoint.routing: azure_deployment`, with the resource URL as `spec.endpoint.base_url`. The SDK's Azure mode then owns the parts that are easy to get wrong: the `Api-Key` header, the `api-version` query (default `2025-04-01-preview`, overridable through `spec.endpoint.query["api-version"]`), and the deployment-path rewriting for the routes its rewrite table covers — `/chat/completions`, `/embeddings`, `/images/generations`, `/images/edits`, `/audio/speech`, `/audio/transcriptions`, `/audio/translations`. `api: responses` is *not* in that table: it is posted to `<base_url>/openai/responses` with the deployment id only in the body, while Azure documents the deployment-scoped `/openai/deployments/{deployment}/responses`. Verify that route against your resource before moving an Azure deployment to the Responses surface |
| `driver/deepseek` | `driver/openai` | `spec.endpoint.base_url` for the compatible surface, `spec.wire` for its dialect differences, `spec.catalog: declared` so its model names never inherit OpenAI facts |
| `driver/kimi` | `driver/openai` | same shape: `base_url` plus `api: chat`, which is the only surface Moonshot serves |
| `driver/qwen` | — | no replacement: there is no DashScope environment to verify a driver against, so the module was removed rather than carried untested |
| `driver/minimax`, `kind: generate` | `driver/anthropic` | `spec.endpoint.base_url` pointing at the `/anthropic` Messages surface |

`driver/minimax` still exists for its native media API — `kind: image`, `tts`,
`music`, `video` and `context_ir`. Only its `generate` kind moved.

## Deployment documents

The old drivers took flat settings (`base_url`, or `endpoint` and
`api_version` for Azure). The new ones split the provider into layers:
`spec.endpoint` (where), `spec.auth` (how the key rides), `spec.wire` (what the
endpoint accepts) and `spec.catalog` (which model namespace it starts from).

Before, a DeepSeek provider was its own module:

```yaml
provider:
  kind: inference.Provider
  impl: deepseek
  settings:
    id: deepseek
    spec:
      base_url: https://api.deepseek.com
      api: responses
      models:
        - name: deepseek-flash
          kind: generate
```

After, it is the OpenAI driver configured for that endpoint:

```yaml
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
```

`impl:` names the driver module; `id:` names the deployment. Requests keep
addressing the deployment, so a graph or route that says
`{provider: deepseek, name: deepseek-flash}` needs no change — the driver
behind that id is what moved. The same split applies to Azure: `impl: openai`
with `routing: azure_deployment`, the resource URL as `endpoint.base_url`, and
the old `api_version` as `endpoint.query["api-version"]` when it differs from
the default.

## Go callers

Imports and factory registrations follow the same move:

| Before | After |
| --- | --- |
| `reg.MustRegister(azure.Factory())` | `reg.MustRegister(openai.Factory())` |
| `reg.MustRegister(deepseek.Factory())` | `reg.MustRegister(openai.Factory())` |
| `reg.MustRegister(kimi.Factory())` | `reg.MustRegister(openai.Factory())` |
| generate through `reg.MustRegister(minimax.Factory())` | `reg.MustRegister(anthropic.Factory())` for generate, `minimax.Factory()` stays for the media kinds |
| `reg.MustRegister(qwen.Factory())` | — |

The `impl:` string in a deployment document must match the module that
registers the factory (`openai`, `anthropic`, `bytedance`, `minimax`), and each
driver's settings schema is the layered one described above — see the
[inference guide](../guides/inference.md) for the full provider reference.

## Published versions

The removed modules keep their last published versions on the module proxy, so
pinned builds keep resolving; they receive no further changes. New work should
target `driver/openai` (the OpenAI wire family, including Azure deployments and
compatible endpoints) and `driver/anthropic` (the Messages family).
