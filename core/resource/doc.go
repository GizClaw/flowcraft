// Package resource defines FlowCraft's resource protocol: the serializable
// DTOs of a deployment document (resources with kinds, settings, and DAG
// dependencies), the typed factory contract every resource module implements,
// and the registry that wires kinds to factories.
//
// # Three roles
//
// A resource module authors one value and implements two methods on it:
//
//	resource.Factory[T]   — Spec() declares the factory's (Kind, Impl), its
//	                        typed dependencies and the item type it exposes;
//	                        New() builds a T.
//
// The assembly layer holds the erased form, [Entry]: the same declaration plus
// the Go type T the factory returns. That type is what lets the deployment
// layer check every dependency edge before a single resource is constructed.
//
// A factory reads a dependency through the [Dep] handle it declared — never by
// name. One type parameter carries both halves:
//
//	var depTools = resource.RequireMany[tool.Source]("tool")
//
//	func (f assemblyFactory) Spec() resource.Spec {
//		return resource.Spec{Kind: ..., Impl: ..., Deps: []resource.Requirement{depTools.Spec()}}
//	}
//
//	func (f assemblyFactory) New(ctx context.Context, in resource.Input) (*tool.Assembly, error) {
//		sources, err := depTools.GetAll(in) // []tool.Source, already checked
//		...
//	}
//
// [Register] is a free function rather than a method because Go has no generic
// methods: only a function can introduce the type parameter that ties
// Factory[T] to the Entry a [Registry] stores.
//
// The package is deliberately free of domain types: no agent, no tool, no
// inference. Concrete resource modules (event, workspace, sandbox, ...)
// implement [Factory] and register themselves; the assembly layer built above
// this package resolves the dependency DAG, checks it, and constructs
// resources in topological order.
//
// Structure, by job rather than by file:
//
//   - the serializable vocabulary (Resources, Resource, Deps, Ref, Kind);
//   - the factory contract and the typed dependency handle (Factory[T],
//     Entry, Register, Spec, Input, Requirement, Dep[T]);
//   - the settings pipeline: strict decoding, ${...} expansion,
//     inline/file/embed subtrees, and the loader that bounds and confines
//     them;
//   - the kind/impl registry, reference resolution, and the dependency graph
//     validation that produces construction order.
package resource
