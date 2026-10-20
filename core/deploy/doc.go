// Package deploy assembles a deployment from a resource+agent document: the
// [Document] DTO (resources forming a dependency DAG, agents binding them) and
// the [Builder], in three phases.
//
//   - [Builder.Plan] is pure. It resolves every kind to a factory, validates
//     that every dependency a document supplies is declared, checks the DAG is
//     acyclic, and checks that every dependency edge is type-correct — a
//     resource factory's build type (or a container's item type, or an
//     external's injected type) must satisfy the consumer's
//     [resource.Requirement]. Every problem is reported at once, and nothing is
//     constructed, so a configuration error is diagnosed without side effects.
//   - [Plan.Build] constructs resources in topological order. Dependency types
//     are already proven, so the only conversions left are the typed reads
//     inside factories and [Resolve]/[Collect] at the call site. On failure
//     every value built so far is closed in reverse order.
//   - [Builder.Wire] attaches observers and hooks, binds agents and hands the
//     assembled deployment to resources implementing [Binder].
//
// [Builder.Build] is Plan plus Build, and [Builder.Deploy] adds Wire. A caller
// that wants a configuration preflight calls Plan directly.
//
// The package imports only core packages: the resource protocol
// ([resource]) and error classification ([errdefs]). It knows no
// concrete resource kind.
package deploy
