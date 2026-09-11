package inference

import "github.com/GizClaw/flowcraft/core/inference/model"

// This file keeps the core/inference spellings of the model declaration
// vocabulary working after the declarations moved to core/inference/model.
// They are compatibility shims for existing callers — drivers, hosts, script
// bridges — so the move itself changed no import path and no behavior.
//
// New code should import core/inference/model directly; that package depends
// only on core/message, while this one carries the whole execution contract.

// Operation is the inference workload a model declaration is written against.
//
// Deprecated: use model.Operation from core/inference/model.
type Operation = model.Operation

// ModelID is the public, credential-free identity of a provider model.
//
// Deprecated: use model.ModelID from core/inference/model.
type ModelID = model.ModelID

// ModelRef combines a model identity with the credential profile that
// resolves it.
//
// Deprecated: use model.ModelRef from core/inference/model.
type ModelRef = model.ModelRef

// ModelStatus is a model's lifecycle status.
//
// Deprecated: use model.ModelStatus from core/inference/model.
type ModelStatus = model.ModelStatus

// ModelLifecycle is a model's retirement metadata.
//
// Deprecated: use model.ModelLifecycle from core/inference/model.
type ModelLifecycle = model.ModelLifecycle

// ReasoningKind declares a model's reasoning control capability.
//
// Deprecated: use model.ReasoningKind from core/inference/model.
type ReasoningKind = model.ReasoningKind

// ReasoningCapability declares a model's reasoning control surface.
//
// Deprecated: use model.ReasoningCapability from core/inference/model.
type ReasoningCapability = model.ReasoningCapability

// ReasoningEffort is the request-side reasoning depth knob.
//
// Deprecated: use model.ReasoningEffort from core/inference/model.
type ReasoningEffort = model.ReasoningEffort

// ModelCapabilities describes the feature bits and content kinds a model
// serves.
//
// Deprecated: use model.ModelCapabilities from core/inference/model.
type ModelCapabilities = model.ModelCapabilities

// ModelLimits declares a model's numeric capacity limits.
//
// Deprecated: use model.ModelLimits from core/inference/model.
type ModelLimits = model.ModelLimits

// ModelDescriptor is a model's public discovery metadata.
//
// Deprecated: use model.ModelDescriptor from core/inference/model.
type ModelDescriptor = model.ModelDescriptor

// CapabilitiesPatch declares capability changes relative to a base model.
//
// Deprecated: use model.CapabilitiesPatch from core/inference/model.
type CapabilitiesPatch = model.CapabilitiesPatch

// ReasoningPatch is the reasoning leaf of a CapabilitiesPatch.
//
// Deprecated: use model.ReasoningPatch from core/inference/model.
type ReasoningPatch = model.ReasoningPatch

// Deprecated: use model.OperationGenerate.
const OperationGenerate = model.OperationGenerate

// Deprecated: use model.OperationEmbed.
const OperationEmbed = model.OperationEmbed

// Deprecated: use model.OperationTranscription.
const OperationTranscription = model.OperationTranscription

// Deprecated: use model.ModelStatusActive.
const ModelStatusActive = model.ModelStatusActive

// Deprecated: use model.ModelStatusDeprecated.
const ModelStatusDeprecated = model.ModelStatusDeprecated

// Deprecated: use model.ModelStatusRetired.
const ModelStatusRetired = model.ModelStatusRetired

// Deprecated: use model.ReasoningNone.
const ReasoningNone = model.ReasoningNone

// Deprecated: use model.ReasoningAlways.
const ReasoningAlways = model.ReasoningAlways

// Deprecated: use model.ReasoningToggle.
const ReasoningToggle = model.ReasoningToggle

// Deprecated: use model.ReasoningMinimal.
const ReasoningMinimal = model.ReasoningMinimal

// Deprecated: use model.ReasoningLow.
const ReasoningLow = model.ReasoningLow

// Deprecated: use model.ReasoningMedium.
const ReasoningMedium = model.ReasoningMedium

// Deprecated: use model.ReasoningHigh.
const ReasoningHigh = model.ReasoningHigh

// Deprecated: use model.ReasoningXHigh.
const ReasoningXHigh = model.ReasoningXHigh
