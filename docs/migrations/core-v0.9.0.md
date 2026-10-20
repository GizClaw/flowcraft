---
layout: default
title: Migrating to core/v0.9.0
---
# Migrating to `core/v0.9.0`

`core/v0.9.0` replaces the stringly-typed assembly protocol with a typed one.
A factory declares the Go type it produces, and a dependency is declared and
read through one handle, so a document is type-checked before anything is
constructed. There are **no compatibility shims**: `DepSpec`, `Result.Value`,
`resource.DeploymentBinder` and `Input.Dep`/`DepsMany` are gone.

## Why

The old protocol wrote each dependency contract twice — once as a string in
`Spec.Deps` (`Type: "tool.Source"`) and once as a type assertion inside the
factory — and checked neither against the other. Errors surfaced only when a
factory ran, one at a time, with hand-written messages.

The new protocol derives both from a type parameter, and the assembly layer
records each factory's output type, so a whole deployment is checked in a pure
phase (`deploy.Plan`) and every bad edge is reported in one pass.

## Factory

`resource.Factory` is generic: `New` returns the value's own type.

```go
// before
func (AssemblyFactory) New(ctx context.Context, in resource.Input) (any, error) {
	values := in.DepsMany("tool")
	sources := make([]Source, 0, len(values))
	for _, value := range values {
		src, ok := value.(Source)
		if !ok {
			return nil, errdefs.Validationf("tool: dep is %T, want tool.Source", value)
		}
		sources = append(sources, src)
	}
	return NewAssembly(sources, opts...)
}

// after
var depSources = resource.RequireMany[tool.Source]("tool")

func (AssemblyFactory) New(ctx context.Context, in resource.Input) (*tool.Assembly, error) {
	sources, err := depSources.GetAll(in)
	if err != nil {
		return nil, err
	}
	return tool.NewAssembly(sources, opts...)
}
```

## Dependency declaration

`Spec.Deps` holds `[]resource.Requirement`. Build each one from the handle that
reads it:

| before | after |
| --- | --- |
| `{Name: "tool", Type: "tool.Source", Required: true, Many: true}` | `resource.RequireMany[tool.Source]("tool").Spec()` |
| `{Name: "target", Type: "inference.Assembly", Required: true}` | `resource.Require[*inference.Assembly]("target").Spec()` |
| `{Name: "inference", Type: "inference.Assembly"}` | `resource.Optional[*inference.Assembly]("inference").Spec()` |
| `ItemType: "memory.System"` | `Items: reflect.TypeFor[*memory.Assembly]()` |

Reading: `in.Dep("x")` becomes `depX.Get(in)` (an error) or `depX.TryGet(in)`
(optional), and `in.DepsMany("x")` becomes `depX.GetAll(in)` or
`depX.GetAllNamed(in)` when the document keys matter.

`Input.deps` is unexported: a factory can only read a dependency it declared.
Test doubles that model untyped wiring instantiate `Factory[any]` and read
`resource.Require[any]("x")`; the plan skips the type check for a producer whose
declared type is the empty interface.

## Registration

A free function, because Go has no generic methods:

```go
resource.Register(r, AssemblyFactory{})   // was r.Register(AssemblyFactory{})
```

## Reading built resources

`Result.Value(name) (any, bool)` is gone. Name the type instead:

```go
bus, err := deploy.Resolve[event.Bus](result, cfg.EventBus)   // one value
binders := deploy.Collect[session.ManagerBinder](result)      // every match, in build order
```

## Post-assembly binding

`resource.DeploymentBinder` moved to the package that calls it, with a typed
view:

```go
func (d *LocalDirectory) BindDeployment(view deploy.View) error
```

`deploy.View` is `Agent` / `AgentNames` / `Names`; a consumer that needs less
declares its own narrower interface and the view satisfies it structurally.

## Externals

An injected value now carries its Go type, so the plan can check the consumer's
requirement against it:

```go
deploy.NewExternal("db", "db.Pool", pool)      // deploy.ExternalResource
runtime.NewExternal("db", "db.Pool", pool)     // runtime.ExternalResource
```

The contract string stays document-facing (it is what a reviewer reads); the
enforced contract is the injected value's type.

## Error messages

Configuration errors are now reported by `Plan`, before construction, and name
the document resource and dep:

```
resources["toolbox"].deps["tool"]: "tools" does not satisfy tool.Source
```

Missing required deps are `errdefs.NotFound` rather than `errdefs.Validation`
when reported by a typed handle; the plan reports them as validation errors
first.
