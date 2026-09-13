// Package model is the model declaration vocabulary: identity (ModelID,
// ModelRef), the discovery surface (ModelDescriptor, ModelCapabilities,
// ModelLimits, ModelLifecycle), and the reasoning contract (ReasoningKind,
// ReasoningCapability, ReasoningEffort). A provider spec declares a model by
// filling these types in: the vocabulary carries no overlay or patch form,
// because a declaration is the whole fact.
//
// This is the only home of that vocabulary: core/inference re-exports it
// through deprecated aliases (see core/inference/model_alias.go) so existing
// callers keep compiling, and the execution contract above it declares
// nothing about models on its own.
//
// # Boundary
//
// In scope: everything a provider spec must name to describe a model, plus
// the Operation enum those declarations are written against. The package
// depends on core/message and on core/utils/ptr — a leaf
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
// One concept per file:
//
//	operation.go    Operation and its validation
//	identity.go     ModelID, ModelRef
//	lifecycle.go    ModelStatus, ModelLifecycle
//	reasoning.go    ReasoningKind, ReasoningEffort, ReasoningCapability,
//	                and the effort validation
//	capabilities.go ModelCapabilities and part-kind validation
//	limits.go       ModelLimits
//	descriptor.go   ModelDescriptor
//
// The declaration types are the same ones descriptors publish, so a driver
// projects a declaration onto discovery metadata without a second DTO.
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
