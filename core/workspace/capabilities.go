package workspace

// Capabilities describes the storage characteristics of a Workspace
// implementation. Higher-layer adapters that need accurate semantics
// (e.g. an LSM-style retrieval index that relies on atomic Rename, or
// a multi-process coordinator that needs to know whether the workspace
// is shareable) read these via [CapabilityReporter] instead of
// hard-coding per-implementation assumptions.
//
// All fields default to false, which is the conservative
// interpretation: adapters that do not bother to type-assert
// CapabilityReporter — or that handle a workspace whose author has
// not yet implemented the interface — see "no guarantees" and pick
// the safe path. Adding a new field is therefore additive.
type Capabilities struct {
	// AtomicRename is true when [Workspace.Rename] succeeds or
	// fails as a single observable operation; readers never see a
	// half-renamed state. POSIX rename(2) on the same filesystem
	// satisfies this. Object stores that emulate Rename as
	// copy+delete do not.
	AtomicRename bool

	// ReadAfterWrite is true when a successful [Workspace.Write]
	// or [Workspace.Append] is immediately observable to a
	// subsequent [Workspace.Read] / [Workspace.List] /
	// [Workspace.Exists] / [Workspace.Stat] from the same client.
	// Strongly-consistent stores (local filesystem, sufficiently
	// recent S3) are true; eventually-consistent stores are false.
	ReadAfterWrite bool

	// DurableOnWrite is true when a successful Write hits stable
	// storage before returning. Implementations that only buffer
	// in user-space (in-memory mocks, write-back caches) report
	// false. Adapters that own crash-recovery logic (WAL replay,
	// manifest swap) read this to decide whether to perform extra
	// flush operations themselves.
	DurableOnWrite bool

	// Distributed is true when more than one process or host can
	// be opened against the same workspace concurrently. Single-
	// process backends (in-memory map, exclusive local directory)
	// report false; cluster file systems and object stores report
	// true. Coordinators use this to choose between a process-
	// local mutex and a distributed lock primitive.
	Distributed bool
}

// CapabilityReporter is an optional interface that [Workspace]
// implementations may satisfy to publish their [Capabilities].
// Adapters that need the information should type-assert; an
// implementation that does not satisfy the interface should be
// treated as a conservative all-false [Capabilities].
//
// Convenience: see [CapabilitiesOf] which performs the type-assert
// + zero-value fallback in one call.
type CapabilityReporter interface {
	Capabilities() Capabilities
}

// CapabilitiesOf returns ws.Capabilities() when ws implements
// [CapabilityReporter], or a zero-value [Capabilities] otherwise.
// nil ws also returns the zero value.
func CapabilitiesOf(ws Workspace) Capabilities {
	if r, ok := ws.(CapabilityReporter); ok {
		return r.Capabilities()
	}
	return Capabilities{}
}
