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

	// dynamic is the live view of runtime-registered targets merged
	// with the deployment snapshot; frozen replaces it once the
	// generation retires so in-flight delegations keep resolving the
	// exact instances that generation served.
	// source retains the bound live view across FreezeTargets so
	// UnfreezeTargets can restore it on reload rollback.
	source  TargetSource
	dynamic TargetSource
	frozen  map[string]*agent.Agent
}

// Deployment is the minimal read-only deployment view needed by
// LocalDirectory. *deploy.Result implements this interface.
type Deployment interface {
	Agent(id string) (*agent.Agent, bool)
	AgentNames() []string
}

// TargetSource is the live view of dynamically registered targets that
// a directory merges with its deployment snapshot. The runtime's agent
// registry implements it, so registration and unregistration take
// effect without re-binding the directory. Implementations must be safe
// for concurrent use.
type TargetSource interface {
	// Dynamic resolves one dynamic target by id. Deployment targets
	// are served from the directory's own snapshot and never returned
	// here. The id is the AgentID — in the runtime path the
	// registration name — and implementations must resolve it as such.
	Dynamic(id string) (*agent.Agent, bool)
	// DynamicNames lists the dynamic target ids (AgentIDs).
	DynamicNames() []string
}

// TargetViewBinder is implemented by directory resources that want the
// runtime's live target source attached. The runtime calls it once per
// generation before the swap.
type TargetViewBinder interface {
	// BindTargetSource attaches the live dynamic source. It is
	// set-once: a second bind returns a Conflict error.
	BindTargetSource(source TargetSource) error
}

// TargetViewFreezer is implemented by directory resources that must pin
// their dynamic view when the generation retires, so in-flight
// delegations keep resolving the instances that generation served.
//
// Generation semantics: the delegation service uses the resolved
// instance for target validation and telemetry only; actual execution
// goes through the session manager's per-generation resolver. After a
// reload the retired directory may therefore validate against a frozen
// instance while the delegated turn starts on the new generation's
// re-bound instance — "the generation is determined at Session.Start",
// matching the runtime's resolver contract.
type TargetViewFreezer interface {
	// FreezeTargets pins the live dynamic source to a fixed instance
	// set. Freezing is a one-shot commit: callers must guarantee that
	// every later failure path can either proceed without the live
	// view or pair the freeze with UnfreezeTargets, or the directory
	// will silently diverge from the runtime registry.
	FreezeTargets(instances map[string]*agent.Agent)
	// UnfreezeTargets restores the live dynamic source after a
	// rollback. Idempotent; safe to call on a never-frozen directory.
	UnfreezeTargets()
}

// NewDirectory creates an unbound directory.
func NewDirectory() *LocalDirectory {
	return &LocalDirectory{}
}

// Bind installs Build's instances. The deployment snapshot is immutable
// after a successful bind and borrows the instances; the result retains
// lifecycle ownership. The directory as a whole is not frozen: the live
// dynamic view may be attached once (BindTargetSource) and pinned on
// generation retirement (FreezeTargets).
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
		target := targetFromAgent(name, instance)
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

// BindTargetSource attaches the live dynamic target view. It is
// set-once: binding twice returns a Conflict error.
func (d *LocalDirectory) BindTargetSource(source TargetSource) error {
	if d == nil {
		return errdefs.Validationf("local delegation directory: nil receiver")
	}
	if isNilInterface(source) {
		return errdefs.Validationf("local delegation directory: nil target source")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dynamic != nil {
		return errdefs.Conflictf("local delegation directory: target source already bound")
	}
	d.source = source
	d.dynamic = source
	return nil
}

// FreezeTargets replaces the live dynamic source with a fixed instance
// set. Idempotent; used when a generation retires. The bound source is
// retained so UnfreezeTargets can restore it.
func (d *LocalDirectory) FreezeTargets(instances map[string]*agent.Agent) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frozen = instances
	d.dynamic = nil
}

// UnfreezeTargets restores the live dynamic source bound by
// BindTargetSource, discarding the frozen view. Idempotent.
func (d *LocalDirectory) UnfreezeTargets() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frozen = nil
	d.dynamic = d.source
}

func targetFromAgent(name string, instance *agent.Agent) Target {
	id := instance.ID
	if id == "" {
		id = name
	}
	target := Target{
		ID:          id,
		Description: instance.Card.Description,
		Modes:       []Mode{ModeSync, ModeAsync},
	}
	if instance.Card.Name != "" {
		target.Metadata = map[string]string{"name": instance.Card.Name}
	}
	return target
}

// BindDeployment implements resource.DeploymentBinder: the directory
// resource binds itself to the assembled deployment once agents are
// ready. Binding is idempotent per instance (see Bind).
func (d *LocalDirectory) BindDeployment(deployment any) error {
	if d == nil {
		return errdefs.Validationf("local delegation directory: nil receiver")
	}
	view, ok := deployment.(Deployment)
	if !ok || isNilInterface(deployment) {
		return errdefs.Validationf(
			"local delegation directory: deployment is not a read-only deployment view")
	}
	return d.Bind(view)
}

// List returns targets in deploy.Result.InstanceNames order followed by
// the dynamic targets (frozen set when retired, otherwise the live
// source) sorted by id.
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
	return append(out, d.dynamicTargetsLocked()...), nil
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
	if target, ok := d.dynamicTargetLocked(id); ok {
		return cloneTarget(target), nil
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
	if d.frozen != nil {
		if instance, ok := d.frozen[id]; ok && instance != nil {
			return instance, nil
		}
	} else if d.dynamic != nil {
		if instance, ok := d.dynamic.Dynamic(id); ok && instance != nil {
			return instance, nil
		}
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

// dynamicTargetsLocked lists the merged dynamic targets: the frozen set
// when the generation retired, otherwise the live source. Deployment
// ids are excluded so the same agent is never listed twice.
func (d *LocalDirectory) dynamicTargetsLocked() []Target {
	if d.frozen != nil {
		names := make([]string, 0, len(d.frozen))
		for name := range d.frozen {
			names = append(names, name)
		}
		slices.Sort(names)
		out := make([]Target, 0, len(names))
		for _, name := range names {
			instance := d.frozen[name]
			if instance == nil {
				continue
			}
			if _, dup := d.byID[targetFromAgent(name, instance).ID]; dup {
				continue
			}
			out = append(out, targetFromAgent(name, instance))
		}
		return out
	}
	if d.dynamic == nil {
		return nil
	}
	names := d.dynamic.DynamicNames()
	out := make([]Target, 0, len(names))
	for _, name := range names {
		instance, ok := d.dynamic.Dynamic(name)
		if !ok || instance == nil {
			continue
		}
		target := targetFromAgent(name, instance)
		if _, dup := d.byID[target.ID]; dup {
			continue
		}
		out = append(out, target)
	}
	return out
}

// dynamicTargetLocked resolves one dynamic target: frozen first, then
// the live source.
func (d *LocalDirectory) dynamicTargetLocked(id string) (Target, bool) {
	if d.frozen != nil {
		if instance, ok := d.frozen[id]; ok && instance != nil {
			return targetFromAgent(id, instance), true
		}
		return Target{}, false
	}
	if d.dynamic == nil {
		return Target{}, false
	}
	instance, ok := d.dynamic.Dynamic(id)
	if !ok || instance == nil {
		return Target{}, false
	}
	return targetFromAgent(id, instance), true
}

func cloneTarget(target Target) Target {
	target.Modes = slices.Clone(target.Modes)
	target.Metadata = maps.Clone(target.Metadata)
	return target
}

var _ Directory = (*LocalDirectory)(nil)
