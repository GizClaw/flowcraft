// Package model is the model declaration vocabulary: identity (ModelID,
// ModelRef), the discovery surface (ModelDescriptor, ModelCapabilities,
// ModelLimits, ModelLifecycle), the reasoning contract (ReasoningKind,
// ReasoningCapability, ReasoningEffort), and the catalog patch language
// (CapabilitiesPatch, ReasoningPatch) deployments use to redeclare built-in
// entries.
//
// # Status: extraction probe
//
// This package is a stand-alone copy of core/inference's model.go and
// model_patch.go, created to measure the split boundary before any of the
// original code moves. Nothing imports it yet; core/inference is unchanged.
// The parity tests in this package pin byte-identical JSON and identical
// validation behavior against core/inference's declarations, so the eventual
// swap (aliases in core/inference, or a direct migration) is provably
// behavior-preserving.
//
// # Boundary
//
// In scope: everything a provider catalog and a deployment spec must name to
// describe a model, plus the Operation enum those declarations are written
// against. The package depends on core/message and on core/utils/ptr — a leaf
// that imports nothing beyond the standard library — for the shared
// defensive-copy helper its Clone methods use; it never imports the inference
// contract that re-exports it.
//
// The enum enumerates only the workloads that have a request/response surface.
// The reserved "realtime" value the original declaration carried — and the ten
// FieldRealtime* ledger constants beside it — are gone: nothing produced or
// consumed them, and a vocabulary that accepts a workload no driver can serve
// invites deployments to declare it (see operation.go).
//
// # Layout
//
// One concept per file, mirroring the original single model.go:
//
//	operation.go    Operation and its validation
//	identity.go     ModelID, ModelRef
//	lifecycle.go    ModelStatus, ModelLifecycle
//	reasoning.go    ReasoningKind, ReasoningEffort, ReasoningCapability,
//	                ReasoningPatch, and the effort validation
//	capabilities.go ModelCapabilities, CapabilitiesPatch, and part-kind
//	                validation
//	limits.go       ModelLimits
//	descriptor.go   ModelDescriptor
//
// Each declaration's patch form lives with the declaration it overrides:
// adding a capability leaf means touching one file, not two. The original
// model_patch.go is therefore dissolved rather than mirrored.
//
// Out of scope, and deliberately left in core/inference:
//
//   - Metadata (execution result envelope). It carries []Decision, so moving
//     it here would make this package depend on the field ledger, while the
//     ledger needs Operation from here — a cycle. Metadata belongs with the
//     ledger/execution contract, not with model declarations.
//   - Request/response types (GenerateRequest, EmbedRequest, ...), the field
//     ledger (FieldID/Decision/CompileReport), errors, usage, the provider
//     SPI, and the Assembly.
package model
