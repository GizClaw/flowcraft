package agent

import "github.com/GizClaw/flowcraft/core/utils/ptr"

// CheckpointCapabilities tells an engine how a host handles [Checkpointer]
// writes, so it can avoid building a record nothing persists and can pick the
// incremental shape when the store understands it.
//
// It is an optional capability rather than a [Host] method: a host that does
// not implement [CheckpointTarget] is assumed to persist full records, which
// is both the previous behaviour and the shape that cannot lose state.
type CheckpointCapabilities struct {
	// Enabled reports whether the host persists anything at all. When false
	// the engine must not snapshot the board or encode a record: there is
	// nowhere for it to go. A host that *declares* this capability and has no
	// store answers false; a host that does not declare it at all keeps the
	// conservative default of true (see [CheckpointCapabilitiesFromHost]).
	Enabled bool

	// Journal is the incremental write surface, or nil when the store can
	// only be handed self-contained records.
	Journal CheckpointJournal
}

// CheckpointTarget is the optional Host capability behind
// [CheckpointCapabilities]. Hosts that persist checkpoints implement it — and
// an engine reads it through [CheckpointCapabilitiesFromHost], which follows
// [HostUnwrapper] decorators.
//
// Engines must not call Checkpoint directly when the capability is available:
// a disabled sink would still pay for the board snapshot argument.
type CheckpointTarget interface {
	CheckpointCapabilities() CheckpointCapabilities
}

// CheckpointCapabilitiesForStore reports what a store can persist: always a
// self-contained record, and incremental appends when it implements
// [CheckpointJournal]. A nil store persists nothing.
//
// It is the store-side counterpart of [CheckpointCapabilitiesFromHost], used by
// [NewCheckpointWriterForStore] and by hosts built around one store.
func CheckpointCapabilitiesForStore(store CheckpointStore) CheckpointCapabilities {
	if ptr.IsNil(store) {
		return CheckpointCapabilities{}
	}
	caps := CheckpointCapabilities{Enabled: true}
	if journal, ok := store.(CheckpointJournal); ok {
		caps.Journal = journal
	}
	return caps
}

// CheckpointCapabilitiesFromHost resolves the checkpoint capability of h,
// following host decorators. A host that does not declare it is treated as
// "persists full records": the safe default, because it can only cost work,
// never durability.
func CheckpointCapabilitiesFromHost(h Host) CheckpointCapabilities {
	target, ok := CapabilityFromHost[CheckpointTarget](h)
	if !ok {
		return CheckpointCapabilities{Enabled: true}
	}
	caps := target.CheckpointCapabilities()
	if !caps.Enabled {
		return CheckpointCapabilities{}
	}
	return caps
}
