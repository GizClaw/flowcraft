// Package v1alpha1 defines the wire schema for the vesseld daemon
// at the vessel.flowcraft.io/v1alpha1 apiVersion. Every type in
// this package is a 1:1 representation of a YAML / JSON document
// the user puts under their --config folder.
//
// Layering rationale:
//
//   - apispec/v1alpha1 holds wire-level structs only. No business
//     logic. No imports outside the standard library and
//     sdk/errdefs (for classified validation errors).
//   - cmd/vesseld/apispec/decode.go routes raw documents to this
//     package by inspecting apiVersion+kind.
//   - cmd/vesseld/resolver translates v1alpha1.X structs into
//     vessel-runtime instances (spec.Spec, []vessel.Option,
//     resolved Probe / ToolPack / EngineFactory closures).
//
// Versioning:
//
//   - This package is alpha. Alpha fields may change or disappear
//     in any minor release without notice.
//   - When a v1alpha2 lands it sits beside this package; the
//     decode router picks the matching decoder; v1alpha1 stays
//     until the daemon drops support (typically two minor releases
//     after v1alpha2 ships in beta).
//
// Test responsibilities:
//
//   - Each kind file ships a *_test.go covering decode round-trip
//     plus validation negatives. Resolver-level tests live in the
//     resolver package; this package only verifies the wire form
//     is decoded as documented.
package v1alpha1
