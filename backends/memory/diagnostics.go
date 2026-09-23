package memory

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/backends/memory/worker"
)

// Diagnostics is a point-in-time health snapshot of one assembly: per-scope
// derivation cursors and cumulative worker counters.
type Diagnostics struct {
	Scopes []worker.ScopeDiagnostics `json:"scopes"`
	Worker worker.Stats              `json:"worker"`
}

// Diagnostics reports the current derivation state. The call performs
// bounded reads (at most one pending item per cursor) and does not mutate
// state.
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
			result.Scopes = append(result.Scopes, value)
		}
	}
	return result, nil
}
