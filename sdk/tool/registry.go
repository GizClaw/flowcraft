package tool

import (
	"sync"
)

// Tool scope constants control visibility in tool_list and /api/tools.
// The scope is registry-level metadata and does NOT appear in Definition.
const (
	// ScopeAgent marks a tool as available to all agents (default).
	ScopeAgent = "agent"
	// ScopePlatform marks a tool as internal to the CoPilot platform.
	// Platform tools are hidden from tool_list and the frontend ToolSelector,
	// but can still be referenced explicitly in an LLM node's tool_names.
	ScopePlatform = "platform"
)

// entry bundles a tool with its registry-level metadata.
type entry struct {
	tool  Tool
	scope string
	owner uint64
}

// Registry is a thread-safe, mutable Catalog of tools plus their
// scope metadata. It deliberately contains no execution path:
// dispatch, timeouts, telemetry, and concurrency policy live in
// Executor and its middleware chain.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]entry
	owner uint64
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]entry)}
}

// Register adds a tool to the registry with the default scope (ScopeAgent).
func (r *Registry) Register(tool Tool) {
	r.RegisterWithScope(tool, ScopeAgent)
}

// RegisterWithScope adds a tool to the registry with the specified scope.
func (r *Registry) RegisterWithScope(tool Tool, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Definition().Name
	r.tools[name] = entry{tool: tool, scope: scope}
}

// RegisterAllIfAbsent atomically registers tools with ScopeAgent only when
// none of their names already exist. It returns an idempotent release function
// that removes only entries still owned by this registration, so a later
// replacement is never deleted accidentally.
func (r *Registry) RegisterAllIfAbsent(tools ...Tool) (release func(), ok bool) {
	names := make([]string, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for i, item := range tools {
		name := item.Definition().Name
		if _, duplicate := seen[name]; duplicate {
			return nil, false
		}
		seen[name] = struct{}{}
		names[i] = name
	}

	r.mu.Lock()
	for _, name := range names {
		if _, exists := r.tools[name]; exists {
			r.mu.Unlock()
			return nil, false
		}
	}
	r.owner++
	owner := r.owner
	for i, name := range names {
		r.tools[name] = entry{tool: tools[i], scope: ScopeAgent, owner: owner}
	}
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, name := range names {
				if current, exists := r.tools[name]; exists && current.owner == owner {
					delete(r.tools, name)
				}
			}
		})
	}, true
}

// Unregister removes a tool by name. Returns true if the tool existed.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tools[name]
	if ok {
		delete(r.tools, name)
	}
	return ok
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	return e.tool, ok
}

// ScopeOf returns the scope of a registered tool, or ScopeAgent if not found.
func (r *Registry) ScopeOf(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.tools[name]; ok {
		return e.scope
	}
	return ScopeAgent
}

// Definitions returns the Definition for every registered tool (all scopes).
func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]Definition, 0, len(r.tools))
	for _, e := range r.tools {
		defs = append(defs, e.tool.Definition())
	}
	return defs
}

// DefinitionsByScope returns only the Definitions matching the given scope.
func (r *Registry) DefinitionsByScope(scope string) []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var defs []Definition
	for _, e := range r.tools {
		if e.scope == scope {
			defs = append(defs, e.tool.Definition())
		}
	}
	return defs
}

// Names returns the names of all registered tools.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	return names
}

// Len returns the number of registered tools.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}
