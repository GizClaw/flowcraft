---
layout: default
title: Deployment Assembly
---
# Deployment Assembly Guide

`core/deploy` assembles one deployment document into shared resources and
runnable agents. It knows only the resource protocol; concrete kinds are
registered by the application.

## Document

```yaml
version: v1

resources:
  events:
    kind: event.Bus
    impl: memory

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

## Settings reference expansion

The deploy builder expands inline scalar references in every settings
subtree — resources, agent engines, and agent hooks — before a factory
decodes them. Supported schemes are `${env:NAME}` (missing variable fails the
build), `${base}` / `${base:rel}` (rooted at the deployment document's base
dir), and `${home}` / `${home:rel}` (plus the `~` / `~/...` shorthand). All
schemes are enabled by default; a custom `resource.ReferenceResolver` can be
injected with `deploy.WithResolver` to add application-specific schemes on
top of the defaults (a same-named custom scheme overrides the built-in one).
Expansion is strict: an unknown scheme, a disabled scheme, or a malformed
reference fails the build. A literal `${` is written as `\${...}`, matching
the graph's `${board:*}` escaping convention — **every** literal `${` in any
settings subtree must be escaped, and a leading `~` / `~/...` expands to the
user home directory. Content materialized from `{file: ...}` / `{embed: ...}`
references is expanded the same way. Deployments that previously relied on
literal `${...}` text or `~` paths must escape/quote them after this change.

The agent board namespace is exempt: `${board:<path>}` (and the escaped
`\${board:<path>}` form, backslash preserved) is deferred to execution time,
where it resolves against the live `agent.Board` — graph node configs and the
script bridge's `board.resolve` both understand it. Deploy-time expansion
never touches board references, so existing graph configs need no escaping.

## Secret stores

Credentials can live in declarative `secret.Store` resources instead of
plaintext settings:

```yaml
resources:
  secret.env:
    kind: secret.Store
    impl: env
    settings:
      id: env
      default: true

  provider:
    kind: inference.Provider
    impl: deepseek
    settings:
      profiles:
        - secrets:
            api_key: ${secret:DEEPSEEK_API_KEY}   # default store
            # api_key: ${secret:env.DEEPSEEK_API_KEY}  # explicit store
```

- Stores are built ahead of every other resource, then assembled into the
  `${secret:...}` scheme, so any resource can reference them (and may declare
  them as deps for explicit ordering).
- `${secret:NAME}` resolves through the default store — exactly one store may
  declare `default: true`. `${secret:ID.NAME}` addresses a store by its `id`
  (falling back to the resource name).
- The store prefix is everything before the first dot; the rest is the secret
  name.
- Missing stores or a NAME-only reference with no default store fail the
  build. Resolution is lazy: a `${secret:...}` reference decodes into a typed
  `resource.Secret` that looks the value up at use time (cached per store for
  one minute), so a missing secret surfaces at request time rather than at
  deploy time. Secret values never appear in error messages, logs, or
  serialized settings — `resource.Secret` always renders as `<secret>`.
- Backends are ordinary resource factories: the built-in `env` store reads
  environment variables and the built-in `file` store reads one file per
  secret under a configured `base` directory (escaping the base is rejected,
  trailing newlines are stripped, and file size is capped). External backends
  (keychain, vault, ...) register their own `secret.Store` impls with zero
  core changes.

agents:
  assistant:
    card:
      name: Assistant
    engine:
      kind: agent.Engine
      impl: graph
      deps:
        inference: infer
      settings:
        graph: {file: ./graphs/assistant.yaml}

runtime:
  event_bus: events
  sessions:
    idle_timeout: 10m
```

Top-level fields:

- `version` is required.
- `resources` is a map of named resource entries.
- `agents` is a map of agent definitions.
- `runtime` is opaque to `deploy`; `core/runtime` decodes it strictly.

## Resource entries

```yaml
<name>:
  kind: event.Bus
  impl: memory
  deps: {bus: other}
  settings: {literal: object}
```

`settings` may be inline content or a whole-subtree `{"file": ...}` /
`{"embed": ...}` reference. `deps` bind declared dependency names to resource
refs. A ref is either `resource` or `resource/item`.

## Build and wire

`Builder.Build` constructs resources in dependency order. `Builder.Wire`
then:

1. wires `resource.Wireable` values;
2. builds agent engines and hooks;
3. binds `resource.DeploymentBinder` values with the completed deployment.

Use `Builder.Deploy` for the convenience of both phases.

```go
reg := resource.NewRegistry()
reg.MustRegister(event.NewFactory())
reg.MustRegister(inference.Factory{})
reg.MustRegister(graphresource.Factory{})

builder := deploy.NewBuilder(reg)
result, err := builder.Deploy(ctx, doc)
if err != nil {
    return err
}
defer result.Close()
```

`Result.Close` closes every built resource value in reverse construction
order, then closes each bound agent (engine and lifecycle hooks). Agents
can also be assembled individually at runtime via the exported
`deploy.BindAgent` — the same path `core/runtime` uses for its live agent
registry (see [runtime.md](runtime.md)).

Caller-owned dependencies can be supplied with
`deploy.WithExternalResources`; they are visible to dependency
resolution but are never constructed, wired, or closed by deploy. The
runtime layer exposes this through its `runtime.external_deps` document
declaration and `runtime.Builder.WithExternalResource` (see
[runtime.md](runtime.md)).

## Layered configuration

`deploy.LoadLayers` loads multiple `Layer` values, merges them in ascending
priority order, and returns provenance. The first layer must be complete;
later layers may be partial. Runtime settings, resource settings, and agent
policy are merged.

See [resource.md](resource.md) for the factory protocol.
