---
layout: default
title: Migrating to core/v0.1.0
---
# Migrating to `core/v0.1.0`

`core/v0.1.0` is the breaking cut that replaces the active `sdk` / `sdkx`
surface with one platform module plus provider and integration modules.
There are **no compatibility shims**.

## Module map

| Old module / package | New module / package |
| --- | --- |
| `github.com/GizClaw/flowcraft/sdk/agent` | `github.com/GizClaw/flowcraft/core/agent` |
| `github.com/GizClaw/flowcraft/sdk/message` | `github.com/GizClaw/flowcraft/core/message` |
| `github.com/GizClaw/flowcraft/sdk/errdefs` | `github.com/GizClaw/flowcraft/core/errdefs` |
| `github.com/GizClaw/flowcraft/sdk/config` | `github.com/GizClaw/flowcraft/core/resource` |
| `github.com/GizClaw/flowcraft/sdk/config/utils` | `github.com/GizClaw/flowcraft/core/utils` |
| `github.com/GizClaw/flowcraft/sdkx/deploy` | `github.com/GizClaw/flowcraft/core/deploy` |
| `github.com/GizClaw/flowcraft/sdkx/runtime` | `github.com/GizClaw/flowcraft/core/runtime` |
| `github.com/GizClaw/flowcraft/sdkx/runtime/session` | `github.com/GizClaw/flowcraft/core/runtime/session` |
| `github.com/GizClaw/flowcraft/sdk/graph` | `github.com/GizClaw/flowcraft/core/graph` |
| `github.com/GizClaw/flowcraft/sdk/graph/nodes` | `github.com/GizClaw/flowcraft/core/graph/nodes` |
| `github.com/GizClaw/flowcraft/sdk/graph/config` | `github.com/GizClaw/flowcraft/core/graph/resource` |
| `github.com/GizClaw/flowcraft/sdk/event` | `github.com/GizClaw/flowcraft/core/event` |
| `github.com/GizClaw/flowcraft/sdk/tool` | `github.com/GizClaw/flowcraft/core/tool` |
| `github.com/GizClaw/flowcraft/sdk/tool/middleware` | `github.com/GizClaw/flowcraft/core/tool/middleware` |
| `github.com/GizClaw/flowcraft/sdkx/tool/mcp` | `github.com/GizClaw/flowcraft/core/tool/mcp` |
| `github.com/GizClaw/flowcraft/sdk/workspace` | `github.com/GizClaw/flowcraft/core/workspace` |
| `github.com/GizClaw/flowcraft/sdkx/workspace/objstore` | `github.com/GizClaw/flowcraft/integrations/objstore` |
| `github.com/GizClaw/flowcraft/sdk/sandbox` | `github.com/GizClaw/flowcraft/core/sandbox` |
| `github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap` | `github.com/GizClaw/flowcraft/integrations/sandbox/bwrap` |
| `github.com/GizClaw/flowcraft/sdkx/sandbox/seatbelt` | `github.com/GizClaw/flowcraft/integrations/sandbox/seatbelt` |
| `github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite` | `github.com/GizClaw/flowcraft/integrations/sqlite` |
| `github.com/GizClaw/flowcraft/sdkx/agent/script/jsrt` | `github.com/GizClaw/flowcraft/core/agent/scriptrt/jsrt` |
| `github.com/GizClaw/flowcraft/sdkx/agent/script/luart` | `github.com/GizClaw/flowcraft/core/agent/scriptrt/luart` |
| `github.com/GizClaw/flowcraft/sdkx/inference/openai` | `github.com/GizClaw/flowcraft/driver/openai` |
| `github.com/GizClaw/flowcraft/sdkx/inference/azure` | `github.com/GizClaw/flowcraft/driver/azure` |
| `github.com/GizClaw/flowcraft/sdkx/inference/anthropic` | `github.com/GizClaw/flowcraft/driver/anthropic` |
| `github.com/GizClaw/flowcraft/sdkx/inference/deepseek` | `github.com/GizClaw/flowcraft/driver/deepseek` |
| `github.com/GizClaw/flowcraft/sdkx/inference/qwen` | `github.com/GizClaw/flowcraft/driver/qwen` |
| `github.com/GizClaw/flowcraft/sdkx/inference/kimi` | `github.com/GizClaw/flowcraft/driver/kimi` |
| `github.com/GizClaw/flowcraft/sdkx/inference/minimax` | `github.com/GizClaw/flowcraft/driver/minimax` |
| `github.com/GizClaw/flowcraft/sdkx/inference/bytedance` | `github.com/GizClaw/flowcraft/driver/bytedance` |

## Breaking changes in deployment documents

- Agent engine dependencies live under `engine.deps`, not top-level
  `agent.deps`.
- `resource.Factory` replaces the old `config.Factory` protocol.
- Runtime configuration is decoded by `core/runtime`, with additional
  session limits such as `max_sessions` and `delivery_concurrency`.
- `resource.DeploymentBinder` provides a post-agent wiring phase for
  resources that need the assembled deployment.

## Getting started

```bash
go get github.com/GizClaw/flowcraft/core@v0.1.0
go get github.com/GizClaw/flowcraft/driver/deepseek@v0.1.0
```

Assemble and run a deployment with:

```go
import (
    "github.com/GizClaw/flowcraft/core/deploy"
    "github.com/GizClaw/flowcraft/core/runtime"
)
```

See [docs/index.md](../index.md) for the current guide index.
