package memory

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/backends/memory/maintain"
	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
	"github.com/GizClaw/flowcraft/backends/memory/worker"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// Diagnostics is a point-in-time health snapshot of one assembly: per-scope
// derivation cursors, the projection lanes' producer identity, and cumulative
// worker counters.
type Diagnostics struct {
	Scopes []ScopeDiagnostics `json:"scopes"`
	Worker worker.Stats       `json:"worker"`
}

// ScopeDiagnostics is one hard scope's derivation state -- the cursors the
// worker reports -- together with the state of its projection lanes. The
// embedded value keeps the derivation fields where they were; the lanes are
// what tells a host that a rebuild would change what the scope answers with.
type ScopeDiagnostics struct {
	worker.ScopeDiagnostics
	Lanes []LaneDiagnostics `json:"lanes,omitempty"`
	// Overlay is the scope's maintenance overlay, when the assembly was built
	// with maintenance: the pass that wrote the stored judgments and whether
	// the policy this deployment runs is applying them.
	Overlay *maintain.OverlayDiagnostics `json:"overlay,omitempty"`
}

// LaneDiagnostics reports one projection lane's producer identity for one
// scope: the version the running lane writes with and the version the stored
// state was stamped with. A lane that holds no state for the scope reports
// Stored false and is not stale -- there is nothing an earlier producer wrote.
type LaneDiagnostics struct {
	Name             string `json:"name"`
	ExpectedProducer string `json:"expected_producer"`
	StoredProducer   string `json:"stored_producer,omitempty"`
	Stored           bool   `json:"stored"`
	Stale            bool   `json:"stale"`
	// OtherGenerations lists the storage names this lane's family holds state
	// under for the scope, excluding the name the running lane reads. A lane
	// whose storage name is derived from its producer identity forks one name
	// per generation, so after an algorithm bump the lane reads a name that
	// was never written: it reports Stored false and not stale, which is what
	// a scope that was never indexed reports too. The state an earlier
	// generation holds is here instead, and Repair is what fills the name this
	// lane reads.
	//
	// A deployment that changed its projection setting or embedding model sees
	// the name it left behind here as well: that state is orphaned the same
	// way, and nothing reads it.
	OtherGenerations []string `json:"other_generations,omitempty"`
}

// Diagnostics reports the current derivation state. The call performs bounded
// reads -- at most one pending item per cursor, one manifest and one name
// listing per lane, and one overlay document per scope -- and does not mutate
// state. The overlay is the read whose size follows what is stored rather than
// what is configured: it carries one entry per judged fact, so its bound is
// the overlay itself.
func (assembly *Assembly) Diagnostics(ctx context.Context) (Diagnostics, error) {
	if assembly == nil {
		return Diagnostics{}, errors.New("memory assembly: assembly is required")
	}
	if ctx == nil {
		return Diagnostics{}, errors.New("memory assembly: context is required")
	}
	result := Diagnostics{}
	if assembly.processor != nil {
		result.Worker = assembly.processor.Stats()
		scopes, err := assembly.catalog.List(ctx)
		if err != nil {
			return Diagnostics{}, err
		}
		for _, scope := range scopes {
			value, err := assembly.processor.Diagnostics(ctx, scope)
			if err != nil {
				return Diagnostics{}, err
			}
			entry := ScopeDiagnostics{ScopeDiagnostics: value}
			if assembly.lanes != nil {
				lanes, err := assembly.lanes(ctx, scope)
				if err != nil {
					return Diagnostics{}, err
				}
				entry.Lanes = lanes
			}
			if assembly.overlay != nil {
				overlay, err := assembly.overlay(ctx, scope)
				if err != nil {
					return Diagnostics{}, err
				}
				entry.Overlay = &overlay
			}
			result.Scopes = append(result.Scopes, entry)
		}
	}
	return result, nil
}

// LaneStatus reports the assembly's projection lanes for one scope. It is the
// per-scope half of Diagnostics, for a host that already knows which scope it
// is asking about.
func (assembly *Assembly) LaneStatus(ctx context.Context, scope corememory.Scope) ([]LaneDiagnostics, error) {
	if assembly == nil || assembly.lanes == nil {
		return nil, errors.New("memory assembly: projection lanes are not configured")
	}
	if ctx == nil {
		return nil, errors.New("memory assembly: context is required")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return assembly.lanes(ctx, scope)
}

// OverlayStatus reports the maintenance overlay of one scope: which pass wrote
// the stored judgments and whether the read path applies them. An overlay an
// earlier version or tuning wrote is ignored rather than applied, and the next
// Maintain pass rewrites it under this deployment's policy. It is the per-scope
// half of Diagnostics, for a host that already knows which scope it is asking
// about.
func (assembly *Assembly) OverlayStatus(
	ctx context.Context,
	scope corememory.Scope,
) (maintain.OverlayDiagnostics, error) {
	if assembly == nil || assembly.overlay == nil {
		return maintain.OverlayDiagnostics{}, errors.New("memory assembly: maintenance is not configured")
	}
	if ctx == nil {
		return maintain.OverlayDiagnostics{}, errors.New("memory assembly: context is required")
	}
	if err := scope.Validate(); err != nil {
		return maintain.OverlayDiagnostics{}, err
	}
	return assembly.overlay(ctx, scope)
}

// VectorLaneStats reports the vector lane's cumulative phase counters and the
// vector tables it currently holds resident, when the assembly was built with
// an embedding model. A harness takes two readings around a run and subtracts
// them to attribute latency inside the lane -- loading a table, embedding the
// query, scanning and sorting -- against the lane's own total. The reading
// changes nothing the lane serves, and the footprint it reports is the
// materialized cache, not what the store holds on disk.
func (assembly *Assembly) VectorLaneStats() (vector.Stats, bool) {
	if assembly == nil || assembly.vectorStats == nil {
		return vector.Stats{}, false
	}
	return assembly.vectorStats(), true
}
