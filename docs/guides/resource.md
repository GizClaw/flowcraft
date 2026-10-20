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
type Factory[T any] interface {
    Spec() Spec
    New(ctx context.Context, in Input) (T, error)
}
```

`T` is the value's Go type. The registry keeps it alongside the factory (as the
entry's build type), which is what lets the deployment layer check every
dependency edge before constructing anything.

A dependency is declared and read through one handle, so the contract and the
read cannot drift:

```go
var depSources = resource.RequireMany[tool.Source]("tool")

func (f assemblyFactory) Spec() resource.Spec {
    return resource.Spec{Kind: "tool.Assembly", Impl: "memory",
        Deps: []resource.Requirement{depSources.Spec()}}
}

func (f assemblyFactory) New(ctx context.Context, in resource.Input) (*tool.Assembly, error) {
    sources, err := depSources.GetAll(in)   // []tool.Source
    ...
}
```

`Require` / `Optional` / `RequireMany` / `OptionalMany` are the declarations;
`Get` / `TryGet` / `GetAll` / `GetAllNamed` are the reads. `Input` has no
name-keyed accessor, so a factory cannot read a dependency it did not declare.
A test double that deliberately models untyped wiring instantiates
`Factory[any]`; the plan then skips that producer's type check.

Factories are registered explicitly in a `resource.Registry` — with the free
function, because Go has no generic methods. There is no global registry.
`Input` carries the factory-owned settings subtree, resolved dependencies, and a
`Loader` for `Source` references.

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
- `deploy.Binder` values receive the fully assembled deployment (a
  `deploy.View`) after agents are bound. This is the phase used by delegation
  directories. The interface lives in `core/deploy`, the package that calls it.

## Minimal factory

```go
type localBusFactory struct{}

func (localBusFactory) Spec() resource.Spec {
    return resource.Spec{Kind: "event.Bus", Impl: "memory"}
}

func (localBusFactory) New(
    ctx context.Context,
    in resource.Input,
) (event.Bus, error) {
    return event.NewMemoryBus(), nil
}

reg := resource.NewRegistry()
resource.MustRegister(reg, localBusFactory{})
```

See [deploy.md](deploy.md) for how `deploy.Builder` consumes this protocol.
