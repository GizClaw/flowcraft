package runtime

import (
	"errors"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// AgentRegistry is the runtime-live, concurrency-safe agent view.
// Dynamically registered agents live in its own map; statically deployed
// agents are served as a read-only fallback from the deployment result.
// It implements session.InstanceResolver, so the session manager resolves
// both kinds through one seam.
type AgentRegistry struct {
	mu       sync.RWMutex
	agents   map[string]*agent.Agent
	deployed *deploy.Result
}

func newAgentRegistry(deployed *deploy.Result) *AgentRegistry {
	return &AgentRegistry{
		agents:   make(map[string]*agent.Agent),
		deployed: deployed,
	}
}

// Instance implements session.InstanceResolver.
func (r *AgentRegistry) Instance(id string) (*agent.Agent, bool) {
	return r.Agent(id)
}

// Agent resolves a dynamically registered agent first, then falls back
// to the deployment snapshot.
func (r *AgentRegistry) Agent(id string) (*agent.Agent, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	instance, ok := r.agents[id]
	r.mu.RUnlock()
	if ok {
		return instance, true
	}
	if r.deployed != nil {
		return r.deployed.Agent(id)
	}
	return nil, false
}

// AgentNames returns the sorted union of dynamically registered and
// deployed agent names.
func (r *AgentRegistry) AgentNames() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	r.mu.RLock()
	for name := range r.agents {
		seen[name] = struct{}{}
	}
	r.mu.RUnlock()
	if r.deployed != nil {
		for _, name := range r.deployed.AgentNames() {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Put registers a dynamically registered agent, rejecting duplicates.
func (r *AgentRegistry) Put(id string, instance *agent.Agent) error {
	if r == nil {
		return errdefs.Validationf("runtime: agent registry is nil")
	}
	if id == "" || instance == nil {
		return errdefs.Validationf(
			"runtime: agent registry Put requires an id and a non-nil agent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[id]; exists {
		return errdefs.Conflictf("runtime: agent %q is already registered", id)
	}
	r.agents[id] = instance
	return nil
}

// Delete removes and returns a dynamically registered agent. Deployed
// agents are not affected and return ok=false.
func (r *AgentRegistry) Delete(id string) (*agent.Agent, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.agents[id]
	if !ok {
		return nil, false
	}
	delete(r.agents, id)
	return instance, true
}

// Close closes every dynamically registered agent. Deployed agents are
// owned (and closed) by deploy.Result, so they are left untouched.
func (r *AgentRegistry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	instances := make([]*agent.Agent, 0, len(r.agents))
	for _, instance := range r.agents {
		instances = append(instances, instance)
	}
	r.mu.RUnlock()

	var errs []error
	for _, instance := range instances {
		if err := instance.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
