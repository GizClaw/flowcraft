package memory

import "github.com/GizClaw/flowcraft/memory/internal/compiler"

// SourceKind identifies a canonical source required by a memory assembly.
type SourceKind = compiler.SourceKind

const (
	SourceMessageLog    SourceKind = compiler.SourceMessageLog
	SourceDocumentStore SourceKind = compiler.SourceDocumentStore
)

// Capability identifies a stable semantic memory capability.
type Capability = compiler.Capability

const (
	CapabilityRecentWindow    Capability = compiler.CapabilityRecentWindow
	CapabilitySummaryDAG      Capability = compiler.CapabilitySummaryDAG
	CapabilityDocumentChunks  Capability = compiler.CapabilityDocumentChunks
	CapabilityMessageIndex    Capability = compiler.CapabilityMessageIndex
	CapabilityEntityFactIndex Capability = compiler.CapabilityEntityFactIndex
)

// Spec declares the sources, capabilities, projections, and stage names needed
// by a memory system without selecting concrete stores, indexes, or services.
type Spec = compiler.Spec

// SourceSpec declares one canonical source dependency.
type SourceSpec = compiler.SourceSpec

// CapabilitySpec declares one semantic view capability.
type CapabilitySpec = compiler.CapabilitySpec

// ProjectionSpec asks the compiler to bind an enabled capability to one
// retrieval namespace.
type ProjectionSpec = compiler.ProjectionRequest

// StageSpec declares a named assembly stage without runtime behavior.
type StageSpec = compiler.StageSpec

// Compile converts a public Spec into a validated runtime assembly.
func Compile(spec Spec) error {
	_, err := compiler.Compile(spec)
	if err != nil {
		return err
	}
	return nil
}
