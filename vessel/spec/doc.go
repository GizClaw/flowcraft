// Package spec defines the declarative schema for FlowCraft
// Vessels — the agent runtime unit that bundles Agent + Memory +
// Tools + Lifecycle into a single managed entity.
//
// spec is library-only: nothing but data types, interfaces, and
// validation. The runtime that brings a [Spec] to life lives in
// the parent vessel/ package — keeping the schema in its own
// subpackage means application code can construct or inspect a
// Spec without paying for the runtime's dependency surface.
//
// # Why a separate (sub)package
//
// Two kinds of code need [Spec]:
//
//  1. Application code building a vessel declaratively. These
//     consumers should be able to construct and inspect a spec
//     without compiling the entire vessel runtime — vessel/spec
//     keeps the dependency surface to sdk/errdefs only so the
//     import cost stays negligible.
//
//  2. Custom probe implementations. They satisfy the [Probe]
//     interface declared here but live in user code; likewise they
//     should not be forced to import the full vessel runtime.
//
// # Layering invariant
//
// vessel/spec sits at the top of the dependency graph. It MAY
// import sdk/errdefs. It MUST NOT import sdk/agent, sdk/engine,
// sdk/event, sdk/history, sdk/kanban, sdk/llm, sdk/tool, or any
// other sdk package. The schema is data-shape, not behaviour; the
// vessel runtime fills in the behaviour.
//
// # Versioning
//
// vessel/spec follows the parent vessel module's SemVer. v0.1.0 of
// the vessel runtime exposes only the fields documented in [Spec];
// fields reserved for later versions (token-budget caps,
// secret/config references, sidecar producer chains) are
// deliberately absent so the schema does not commit to behaviour
// the runtime cannot yet honour. They will be additively
// introduced when the corresponding runtime support lands.
package spec
