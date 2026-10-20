---
layout: default
title: Migrating to one driver module
---
# Migrating to one driver module

The provider adapters were consolidated from six Go modules into one:
`driver/anthropic`, `driver/bytedance`, `driver/dashscope`, `driver/minimax`,
`driver/openai` and `driver/typesafe` are now packages of
`github.com/GizClaw/flowcraft/driver`.

## What moved

Nothing on disk moved. The packages keep their directories and their import
paths, so a Go caller changes one line of `go.mod` and no `import` at all:

| Before | After |
| --- | --- |
| `.../driver/anthropic` | `.../driver` |
| `.../driver/bytedance` | `.../driver` |
| `.../driver/dashscope` | `.../driver` |
| `.../driver/minimax` | `.../driver` |
| `.../driver/openai` | `.../driver` |
| `.../driver/typesafe` | `.../driver` |

```go
import "github.com/GizClaw/flowcraft/driver/openai" // unchanged
```

```go
require github.com/GizClaw/flowcraft/driver v0.1.0
```

Every adapter is now one module with one version. Bumping a single driver
moves the version that all of them report, so a deployment that pins `driver`
gets the provider set as a unit rather than six independently moving modules.

## Dependency graph

The merged `go.mod` is the union of the six, at the highest version each path
had selected: the only divergence was `github.com/tidwall/gjson`, which the
OpenAI adapter pinned at v1.19.0 and the Anthropic adapter at v1.18.0, and the
merged module selects v1.19.0. That is also the trade-off the consolidation
makes: a shared transitive dependency is now one version for every adapter, so
a bump forced by one SDK is taken by the code paths of the other five. Where
the adapters genuinely disagree — an Anthropic-only, ByteDance-only or
OpenAI-only requirement — nothing changes, because the module still resolves
those only through the adapter that imports them.

## Published versions

The six module paths keep their last published versions on the module proxy, so
a pinned build keeps resolving and keeps compiling exactly the code it pinned;
they receive no further changes. The `driver/<name>/vX.Y.Z` tags stay where they
are, and no new one is created: further work ships as `driver/vX.Y.Z`.

## Deployment documents

Nothing changes. `impl:` names the adapter (`openai`, `anthropic`, `bytedance`,
`minimax`, `dashscope`, `typesafe`) and that id is a package-level fact, not a
module one, so every `provider:` block, model declaration and route keeps its
meaning.

## Why one module

The adapters share one contract (`core/inference`), one test vocabulary and, for
the most part, one dependency set, and each was a separate module only because
each started as one. The cost was in the release path: a `core` contract change
meant six `go.mod` files to re-pin, six `GOWORK=off` gates to run and six tags to
cut before a deployment could use the change, and `driver/openai` alone could not
be bumped without also deciding what to do with the five it does not touch. The
single module turns that into one re-pin, one gate and one tag.
