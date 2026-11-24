package policy

import (
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// DelegationMode is how a delegated run relates to its parent.
type DelegationMode string

const (
	// DelegationSync waits for the child's result inside the parent's run.
	DelegationSync DelegationMode = "sync"
	// DelegationAsync hands the child off and reports a handle instead.
	DelegationAsync DelegationMode = "async"
)

// Delegation is the authority to hand work to another agent.
//
// A nil Delegation means "may not delegate at all" — the zero value is the safe
// one, because delegation is the one capability that can manufacture a new run
// with a new grant. An empty Targets list inside a non-nil Delegation is also
// "nothing": naming a target is how delegation is granted, and there is no
// sensible "all agents" spelling when an agent's name is not a resource the
// caller necessarily knows.
type Delegation struct {
	// Targets lists the agent names this grant may delegate to.
	Targets []string `json:"targets,omitempty" yaml:"targets,omitempty"`
	// Modes lists the permitted modes.
	Modes []DelegationMode `json:"modes,omitempty" yaml:"modes,omitempty"`
	// MaxDepth caps how many delegation edges may sit above a leaf run.
	MaxDepth int `json:"max_depth,omitempty" yaml:"max_depth,omitempty"`
}

// Validate rejects delegation that cannot be honoured.
func (d Delegation) Validate() error {
	if len(d.Targets) == 0 {
		return errdefs.Validationf("policy.delegation: at least one target is required")
	}
	if len(d.Modes) == 0 {
		return errdefs.Validationf("policy.delegation: at least one mode is required")
	}
	for i, target := range d.Targets {
		if strings.TrimSpace(target) == "" {
			return errdefs.Validationf("policy.delegation.targets[%d] is empty", i)
		}
	}
	for i, mode := range d.Modes {
		switch mode {
		case DelegationSync, DelegationAsync:
		default:
			return errdefs.Validationf("policy.delegation.modes[%d]: unknown mode %q", i, mode)
		}
	}
	if d.MaxDepth < 0 {
		return errdefs.Validationf("policy.delegation: max_depth must not be negative")
	}
	return nil
}

// AllowsTarget reports whether name may be delegated to.
func (d *Delegation) AllowsTarget(name string) bool {
	return d != nil && slices.Contains(d.Targets, name)
}

// AllowsMode reports whether mode may be used.
func (d *Delegation) AllowsMode(mode DelegationMode) bool {
	return d != nil && slices.Contains(d.Modes, mode)
}

// Clone returns an independent copy, so a narrowed grant never shares a slice
// with its ceiling.
func (d *Delegation) Clone() *Delegation {
	if d == nil {
		return nil
	}
	out := *d
	out.Targets = slices.Clone(d.Targets)
	out.Modes = slices.Clone(d.Modes)
	return &out
}
