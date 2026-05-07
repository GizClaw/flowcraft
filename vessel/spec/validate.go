package spec

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Validate returns nil when the spec is internally consistent and
// satisfies the constraints v0.1.0 of the vessel runtime enforces.
// Errors are classified with sdk/errdefs so callers can surface
// them uniformly through HTTP / gRPC / CLI layers.
//
// Validate is cheap and side-effect-free: callers MAY invoke it
// inside admission webhooks, CLI parsers, or right before
// vessel.New. The vessel runtime calls it itself at New time, so a
// pre-flight Validate is optional but useful for surfacing errors
// before the runtime starts touching dependencies.
func (s Spec) Validate() error {
	if len(s.Agents) == 0 {
		return errdefs.Validationf("vesselspec: Spec.Agents must contain at least one entry")
	}

	seen := make(map[string]struct{}, len(s.Agents))
	for i, a := range s.Agents {
		if a.Name == "" {
			return errdefs.Validationf("vesselspec: Agents[%d].Name is empty", i)
		}
		if _, dup := seen[a.Name]; dup {
			return errdefs.Validationf("vesselspec: Agents[%d].Name %q is duplicated", i, a.Name)
		}
		seen[a.Name] = struct{}{}

		switch a.HistoryAccess {
		case "", HistoryAccessNone, HistoryAccessReadOnly, HistoryAccessReadWrite:
		default:
			return errdefs.Validationf("vesselspec: Agents[%d].HistoryAccess %q is invalid", i, a.HistoryAccess)
		}

		if a.Sidecar && a.SubscribeTo == "" {
			return errdefs.Validationf("vesselspec: Agents[%d].SubscribeTo is required when Sidecar=true", i)
		}
		if !a.Sidecar && a.SubscribeTo != "" {
			return errdefs.Validationf("vesselspec: Agents[%d].SubscribeTo set but Sidecar=false (would be silently ignored)", i)
		}

		if a.Dispatcher && s.Kanban == nil {
			return errdefs.Validationf("vesselspec: Agents[%d].Dispatcher=true requires Spec.Kanban to be set", i)
		}
		if a.ProducerChain < 0 {
			return errdefs.Validationf("vesselspec: Agents[%d].ProducerChain must be >= 0", i)
		}
	}

	if s.History != nil {
		switch s.History.Kind {
		case "", "buffer", "compacted":
		default:
			return errdefs.Validationf("vesselspec: History.Kind %q is invalid (want \"buffer\" or \"compacted\")", s.History.Kind)
		}
		if s.History.MaxMessages < 0 {
			return errdefs.Validationf("vesselspec: History.MaxMessages must be >= 0")
		}
		if s.History.TokenBudget < 0 {
			return errdefs.Validationf("vesselspec: History.TokenBudget must be >= 0")
		}
	}

	if s.Resources.MaxConcurrentRuns < 0 {
		return errdefs.Validationf("vesselspec: Resources.MaxConcurrentRuns must be >= 0")
	}
	if s.Resources.TurnTimeout < 0 {
		return errdefs.Validationf("vesselspec: Resources.TurnTimeout must be >= 0")
	}
	if s.Resources.MaxTokensPerTurn < 0 {
		return errdefs.Validationf("vesselspec: Resources.MaxTokensPerTurn must be >= 0")
	}
	if s.Resources.MaxTokensPerHour < 0 {
		return errdefs.Validationf("vesselspec: Resources.MaxTokensPerHour must be >= 0")
	}

	switch s.Restart.Mode {
	case "", RestartNever, RestartOnFailure:
	default:
		return errdefs.Validationf("vesselspec: Restart.Mode %q is invalid (v0.1.0 supports \"never\" or \"on_failure\")", s.Restart.Mode)
	}
	if s.Restart.MaxRestarts < 0 {
		return errdefs.Validationf("vesselspec: Restart.MaxRestarts must be >= 0")
	}
	if s.Restart.BackoffInit < 0 {
		return errdefs.Validationf("vesselspec: Restart.BackoffInit must be >= 0")
	}
	if s.Restart.BackoffMax < 0 {
		return errdefs.Validationf("vesselspec: Restart.BackoffMax must be >= 0")
	}

	if s.Kanban != nil {
		if s.Kanban.MaxPendingTasks < 0 {
			return errdefs.Validationf("vesselspec: Kanban.MaxPendingTasks must be >= 0")
		}
		if s.Kanban.MaxProducerChain < 0 {
			return errdefs.Validationf("vesselspec: Kanban.MaxProducerChain must be >= 0")
		}
		if s.Kanban.CallbackMaxSummary < 0 {
			return errdefs.Validationf("vesselspec: Kanban.CallbackMaxSummary must be >= 0")
		}
	}

	if s.Probes != nil {
		for i, p := range s.Probes.Liveness {
			if p == nil {
				return errdefs.Validationf("vesselspec: Probes.Liveness[%d] is nil", i)
			}
			if p.Name() == "" {
				return errdefs.Validationf("vesselspec: Probes.Liveness[%d].Name() is empty", i)
			}
		}
		if s.Probes.Interval < 0 {
			return errdefs.Validationf("vesselspec: Probes.Interval must be >= 0")
		}
		if s.Probes.Timeout < 0 {
			return errdefs.Validationf("vesselspec: Probes.Timeout must be >= 0")
		}
		if s.Probes.FailureThreshold < 0 {
			return errdefs.Validationf("vesselspec: Probes.FailureThreshold must be >= 0")
		}
	}

	return nil
}

// Agent returns a pointer to the Agent entry with the given name,
// or nil + false when no such agent exists. It is the small lookup
// helper the Captain uses on every Submit; exposing it lets caller-
// side validators reuse the same code path.
func (s Spec) Agent(name string) (*Agent, bool) {
	for i := range s.Agents {
		if s.Agents[i].Name == name {
			return &s.Agents[i], true
		}
	}
	return nil, false
}
