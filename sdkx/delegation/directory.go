package delegation

import (
	"context"
	"maps"
	"slices"
	"sync"

	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
)

// Directory exposes the agents assembled by deploy.Build as local delegation
// targets. It may be constructed before Build and bound exactly once after it.
type Directory struct {
	mu      sync.RWMutex
	bound   bool
	targets []sdkdelegation.Target
	byID    map[string]sdkdelegation.Target
	lookup  map[string]*deploy.Instance
}

// Deployment is the minimal read-only deployment view needed by Directory.
// *deploy.Result implements this interface, preserving existing callers.
type Deployment interface {
	Instance(id string) (*deploy.Instance, bool)
	InstanceNames() []string
}

// NewDirectory creates an unbound directory.
func NewDirectory() *Directory {
	return &Directory{}
}

// Bind installs Build's instances. A directory is immutable after a successful
// bind and borrows the instances; the result retains lifecycle ownership.
func (d *Directory) Bind(result Deployment) error {
	if d == nil {
		return errdefs.Validationf("local delegation directory: nil receiver")
	}
	if isNilInterface(result) {
		return errdefs.Validationf("local delegation directory: nil deployment")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bound {
		return errdefs.Conflictf("local delegation directory: already bound")
	}

	names := result.InstanceNames()
	targets := make([]sdkdelegation.Target, 0, len(names))
	byID := make(map[string]sdkdelegation.Target, len(names))
	lookup := make(map[string]*deploy.Instance, len(names))
	for _, name := range names {
		instance, ok := result.Instance(name)
		if !ok || instance == nil {
			return errdefs.Internalf("local delegation directory: deploy result listed missing instance %q", name)
		}
		id := instance.Agent.ID
		if id == "" {
			id = name
		}
		if _, duplicate := byID[id]; duplicate {
			return errdefs.Conflictf("local delegation directory: duplicate target id %q", id)
		}
		target := sdkdelegation.Target{
			ID:          id,
			Description: instance.Agent.Card.Description,
			Modes: []sdkdelegation.Mode{
				sdkdelegation.ModeSync,
				sdkdelegation.ModeHandoff,
				sdkdelegation.ModeAsync,
			},
		}
		if instance.Agent.Card.Name != "" {
			target.Metadata = map[string]string{"name": instance.Agent.Card.Name}
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
func (d *Directory) List(context.Context) ([]sdkdelegation.Target, error) {
	if d == nil {
		return nil, errdefs.NotAvailablef("local delegation directory: nil receiver")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return nil, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	out := make([]sdkdelegation.Target, len(d.targets))
	for i, target := range d.targets {
		out[i] = cloneTarget(target)
	}
	return out, nil
}

// Get returns one target by id.
func (d *Directory) Get(_ context.Context, id string) (sdkdelegation.Target, error) {
	if d == nil {
		return sdkdelegation.Target{}, sdkdelegation.TargetNotFound(id)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return sdkdelegation.Target{}, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	target, ok := d.byID[id]
	if !ok {
		return sdkdelegation.Target{}, sdkdelegation.TargetNotFound(id)
	}
	return cloneTarget(target), nil
}

// Lookup resolves a target to the runnable deploy instance.
func (d *Directory) Lookup(_ context.Context, id string) (*deploy.Instance, error) {
	if d == nil {
		return nil, sdkdelegation.TargetNotFound(id)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.bound {
		return nil, errdefs.NotAvailablef("local delegation directory: not bound")
	}
	instance, ok := d.lookup[id]
	if !ok {
		return nil, sdkdelegation.TargetNotFound(id)
	}
	if instance == nil {
		return nil, errdefs.Internalf("local delegation directory: target %q has no deploy instance", id)
	}
	return instance, nil
}

func cloneTarget(target sdkdelegation.Target) sdkdelegation.Target {
	target.Modes = slices.Clone(target.Modes)
	target.Metadata = maps.Clone(target.Metadata)
	return target
}

var _ sdkdelegation.Directory = (*Directory)(nil)
