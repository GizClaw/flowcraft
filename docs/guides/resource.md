---
layout: default
title: Resource Protocol
---
# Resource Protocol Guide

`core/resource` is the provider-neutral assembly protocol used by every
deployment resource and agent engine/hook factory.

## Concepts

### Kind, Impl, and Ref

`Kind` names a resource category (`event.Bus`, `workspace.Workspace`,
`tool.Assembly`, `agent.Engine`). `Impl` selects one registered
implementation. A `Ref` addresses a whole resource (`infer`) or one item
exported by a container resource (`fs/project`).

### Factory and Registry

```go
type Factory interface {
    Spec() Spec
    New(ctx context.Context, in Input) (any, error)
}
```

Factories are registered explicitly in a `resource.Registry`. There is no
global registry. `Input` carries the factory-owned settings subtree, resolved
dependencies, and a `Loader` for `Source` references.

### Dependency DAG

`resource.Graph` validates named resources, resolves dependency refs, rejects
cycles, and returns a stable topological order. `deploy.Builder` uses that
order to construct resources and to close them in reverse order on failure;
`deploy.Result.Close` also closes each bound agent (engine and hooks).

### Loader and Source

`Source` represents inline content or a whole-subtree `{"file": ...}` /
`{"embed": ...}` reference. `Loader` materializes references with a base-dir
and size cap; file references are confined to the base directory.

### Wire and deployment binding

- `resource.Wireable` values attach observers/hooks after construction.
- `resource.DeploymentBinder` values receive the fully assembled deployment
  after agents are bound. This is the phase used by delegation directories.

## Minimal factory

```go
type localBusFactory struct{}

func (localBusFactory) Spec() resource.Spec {
    return resource.Spec{Kind: "event.Bus", Impl: "memory"}
}

func (localBusFactory) New(
    ctx context.Context,
    in resource.Input,
) (any, error) {
    return event.NewMemoryBus(), nil
}

reg := resource.NewRegistry()
reg.MustRegister(localBusFactory{})
```

See [deploy.md](deploy.md) for how `deploy.Builder` consumes this protocol.
