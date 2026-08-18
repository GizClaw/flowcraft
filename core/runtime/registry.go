package runtime

import (
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// dynamicAgentEntry is the bookkeeping record of one dynamically
// registered agent: the assembled instance plus everything Reload needs
// to re-bind it against a new generation.
type dynamicAgentEntry struct {
	instance     *agent.Agent
	definition   agent.Definition
	toolAssembly string // WithToolAssembly resource name; "" = none/default
}

// AgentRegistry is the runtime-live, concurrency-safe agent view.
// Dynamically registered agents live in its own map; statically deployed
// agents are served as a read-only fallback from the deployment result.
// It implements session.InstanceResolver, so the session manager resolves
// both kinds through one seam.
type AgentRegistry struct {
	mu       sync.RWMutex
	agents   map[string]dynamicAgentEntry
	deployed *deploy.Result
}

func newAgentRegistry(deployed *deploy.Result) *AgentRegistry {
	return &AgentRegistry{
		agents:   make(map[string]dynamicAgentEntry),
		deployed: deployed,
	}
}

// Agent resolves a dynamically registered agent first, then falls back
// to the deployment snapshot.
func (r *AgentRegistry) Agent(id string) (*agent.Agent, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	entry, ok := r.agents[id]
	r.mu.RUnlock()
	if ok {
		return entry.instance, true
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

// Dynamic returns the live instance registered under id, or ok=false.
// It is the dynamic-only view used by per-generation resolvers.
func (r *AgentRegistry) Dynamic(id string) (*agent.Agent, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	entry, ok := r.agents[id]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return entry.instance, true
}

// Entries returns a defensive copy of every dynamic registration.
func (r *AgentRegistry) Entries() map[string]dynamicAgentEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]dynamicAgentEntry, len(r.agents))
	for id, entry := range r.agents {
		out[id] = entry
	}
	return out
}

// Replace atomically swaps the dynamic view and the deployed fallback.
// It is the Reload commit path; callers must hold lifecycleMu.
func (r *AgentRegistry) Replace(
	entries map[string]dynamicAgentEntry,
	deployed *deploy.Result,
) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.agents = entries
	r.deployed = deployed
	r.mu.Unlock()
}

// dynamicInstances projects a registry entry snapshot into the
// instance-only map a generation adopts at swap/close time.
func dynamicInstances(entries map[string]dynamicAgentEntry) map[string]*agent.Agent {
	out := make(map[string]*agent.Agent, len(entries))
	for name, entry := range entries {
		out[name] = entry.instance
	}
	return out
}

// Put registers a dynamically registered agent, rejecting duplicates.
func (r *AgentRegistry) Put(id string, entry dynamicAgentEntry) error {
	if r == nil {
		return errdefs.Validationf("runtime: agent registry is nil")
	}
	if id == "" || entry.instance == nil {
		return errdefs.Validationf(
			"runtime: agent registry Put requires an id and a non-nil agent")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.agents[id]; exists {
		return errdefs.Conflictf("runtime: agent %q is already registered", id)
	}
	r.agents[id] = entry
	return nil
}

// Delete removes and returns a dynamically registered entry. Deployed
// agents are not affected and return ok=false.
func (r *AgentRegistry) Delete(id string) (dynamicAgentEntry, bool) {
	if r == nil {
		return dynamicAgentEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.agents[id]
	if !ok {
		return dynamicAgentEntry{}, false
	}
	delete(r.agents, id)
	return entry, true
}

// Close is retained for API compatibility but is a no-op: dynamic
// instances are owned by the generation that adopted them and closed by
// Generation.close, so closing them here would double-close.
func (r *AgentRegistry) Close() error {
	return nil
}
