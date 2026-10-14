package memory

import (
	"context"
	"errors"

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
}

// Diagnostics reports the current derivation state. The call performs
// bounded reads (at most one pending item per cursor, one manifest per lane)
// and does not mutate state.
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
