// Package resource defines FlowCraft's resource protocol: the
// serializable DTOs of a deployment document (resources with kinds,
// settings, and DAG dependencies), the Factory contract every resource
// module implements, and the registry that wires kinds to factories.
//
// The package is deliberately free of domain types: no agent, no tool,
// no inference. Concrete resource modules (event, workspace, sandbox,
// ...) implement [Factory] and register themselves; the assembly layer
// built above this package resolves the dependency DAG and constructs
// resources in topological order.
//
// Files:
//
//   - dto.go      — Document / Resources / Resource / Deps / Ref / Kind
//   - dag.go      — dependency-graph validation and topological order
//   - spec.go     — Factory / Spec / DepSpec / Input
//   - settings.go — strict settings decoding
//   - expand.go   — scalar settings reference expansion (env, base, home)
//   - source.go   — Source: inline / {file:} / {embed:} settings subtrees
//   - loader.go   — Loader: base dir, embed FS, confinement, size caps
//   - registry.go — kind/impl registry
package resource
