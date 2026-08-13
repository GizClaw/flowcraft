package delegation

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Directory exposes the agents assembled by deploy.Build as local delegation
// targets. It may be constructed before Build and bound exactly once after it.
type LocalDirectory struct {
	mu      sync.RWMutex
	bound   bool
	targets []Target
	byID    map[string]Target
	lookup  map[string]*agent.Agent
}

// Deployment is the minimal read-only deployment view needed by
// LocalDirectory. *deploy.Result implements this interface.
type Deployment interface {
	Agent(id string) (*agent.Agent, bool)
	AgentNames() []string
}

// NewDirectory creates an unbound directory.
func NewDirectory() *LocalDirectory {
	return &LocalDirectory{}
}

// Bind installs Build's instances. A directory is immutable after a successful
// bind and borrows the instances; the result retains lifecycle ownership.
func (d *LocalDirectory) Bind(result Deployment) error {
	if d == nil {
		return errdefs.Validationf("local delegation directory: nil receiver")
	}
	if isNilInterface(result) {
		return errdefs.Validationf("local delegation directory: nil deployment")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bound {
		// Binding is idempotent: a directory is immutable after a
		// successful bind, so multiple resources may share one
		// directory without failing the deployment.
		return nil
	}

	names := result.AgentNames()
	targets := make([]Target, 0, len(names))
	byID := make(map[string]Target, len(names))
	lookup := make(map[string]*agent.Agent, len(names))
	for _, name := range names {
		instance, ok := result.Agent(name)
		if !ok || instance == nil {
			return errdefs.Internalf("local delegation directory: deploy result listed missing agent %q", name)
		}
		id := instance.ID
		if id == "" {
			id = name
		}
		if _, duplicate := byID[id]; duplicate {
			return errdefs.Conflictf("local delegation directory: duplicate target id %q", id)
		}
		target := Target{
			ID:          id,
			Description: instance.Card.Description,
			Modes:       []Mode{ModeSync, ModeAsync},
		}
		if instance.Card.Name != "" {
			target.Metadata = map[string]string{"name": instance.Card.Name}
		}
		targets = append(targets, target)
		byID[id] = target
		lookup[id] = instance
	}

	d.targets = targets
	d.byID = byID
	d.lookup = lookup
	d.bound = true
	return nil
}

// List returns targets in deploy.Result.InstanceNames order.
func (d *LocalDirectory) List(context.Context) ([]Target, error) {
	if d == nil {
		return nil, errdefs.NotAvailablef("local delegation directory: nil receiver")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return nil, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	out := make([]Target, len(d.targets))
	for i, target := range d.targets {
		out[i] = cloneTarget(target)
	}
	return out, nil
}

// Get returns one target by id.
func (d *LocalDirectory) Get(_ context.Context, id string) (Target, error) {
	if d == nil {
		return Target{}, TargetNotFound(id)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return Target{}, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	target, ok := d.byID[id]
	if !ok {
		return Target{}, TargetNotFound(id)
	}
	return cloneTarget(target), nil
}

// Lookup resolves a target to the runnable agent.
func (d *LocalDirectory) Lookup(_ context.Context, id string) (*agent.Agent, error) {
	if d == nil {
		return nil, TargetNotFound(id)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return nil, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	instance, ok := d.lookup[id]
	if !ok {
		return nil, TargetNotFound(id)
	}
	if instance == nil {
		return nil, errdefs.Internalf("local delegation directory: target %q has no deploy instance", id)
	}
	return instance, nil
}

func cloneTarget(target Target) Target {
	target.Modes = slices.Clone(target.Modes)
	target.Metadata = maps.Clone(target.Metadata)
	return target
}

var _ Directory = (*LocalDirectory)(nil)
