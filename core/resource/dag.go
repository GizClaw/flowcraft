package resource

import (
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Graph is the resource dependency DAG of one deployment document.
// Nodes are named resources; edges come from each resource's Deps.
type Graph struct {
	nodes    map[string]Resource
	external map[string]struct{}
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		nodes:    make(map[string]Resource),
		external: make(map[string]struct{}),
	}
}

// Add inserts a named resource, rejecting duplicates and invalid
// resources.
func (g *Graph) Add(name string, res Resource) error {
	if _, dup := g.nodes[name]; dup {
		return errdefs.Conflictf("resource graph: duplicate node %q", name)
	}
	if _, dup := g.external[name]; dup {
		return errdefs.Conflictf("resource graph: node %q duplicates an external node", name)
	}
	if err := res.Validate(); err != nil {
		return errdefs.Validationf("resource graph: node %q: %v", name, err)
	}
	g.nodes[name] = res
	return nil
}

// AddExternal inserts an externally supplied leaf dependency. External
// nodes may be referenced by document resources but are never built,
// wired, or returned by TopoOrder.
func (g *Graph) AddExternal(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") {
		return errdefs.Validationf(
			"resource graph: external node %q must be a non-empty resource name without '/'", name)
	}
	if _, dup := g.nodes[name]; dup {
		return errdefs.Conflictf(
			"resource graph: external node %q duplicates a resource", name)
	}
	if _, dup := g.external[name]; dup {
		return errdefs.Conflictf(
			"resource graph: duplicate external node %q", name)
	}
	g.external[name] = struct{}{}
	return nil
}

// Node returns the resource registered under name.
func (g *Graph) Node(name string) (Resource, bool) {
	res, ok := g.nodes[name]
	return res, ok
}

// Names returns the sorted node names.
func (g *Graph) Names() []string {
	names := make([]string, 0, len(g.nodes))
	for name := range g.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Validate checks every node, resolves every dep ref against the node
// set (dangling refs), and rejects dependency cycles.
func (g *Graph) Validate() error {
	for name, res := range g.nodes {
		if err := res.Validate(); err != nil {
			return errdefs.Validationf("resource graph: node %q: %v", name, err)
		}
		for depName, ref := range res.Deps {
			refName := ref.ResourceName()
			if _, ok := g.nodes[refName]; !ok {
				if _, ok := g.external[refName]; !ok {
					return errdefs.Validationf(
						"resource graph: node %q dep %q references missing resource %q",
						name, depName, refName)
				}
			}
		}
	}
	if cycle := g.findCycle(); cycle != nil {
		return errdefs.Validationf("resource graph: dependency cycle: %v", cycle)
	}
	return nil
}

// TopoOrder returns node names in dependency order (dependencies
// first). A cycle is a validation error.
func (g *Graph) TopoOrder() ([]string, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	indegree := make(map[string]int, len(g.nodes))
	dependents := make(map[string][]string, len(g.nodes))
	for name, res := range g.nodes {
		indegree[name] = len(res.Deps)
		for _, ref := range res.Deps {
			dependents[ref.ResourceName()] = append(
				dependents[ref.ResourceName()], name)
		}
	}
	ready := make([]string, 0, len(g.nodes)+len(g.external))
	for name := range g.external {
		ready = append(ready, name)
	}
	for name, degree := range indegree {
		if degree == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		if _, external := g.external[name]; external {
			for _, dependent := range dependents[name] {
				indegree[dependent]--
				if indegree[dependent] == 0 {
					ready = append(ready, dependent)
					sort.Strings(ready)
				}
			}
			continue
		}
		order = append(order, name)
		for _, dependent := range dependents[name] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	return order, nil
}

// findCycle returns one cycle as a path, or nil. It uses iterative
// DFS over dependency edges.
func (g *Graph) findCycle() []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	state := make(map[string]int, len(g.nodes))
	var stack []string
	var dfs func(name string) []string
	dfs = func(name string) []string {
		state[name] = gray
		stack = append(stack, name)
		for _, ref := range g.nodes[name].Deps {
			next := ref.ResourceName()
			if _, external := g.external[next]; external {
				continue
			}
			switch state[next] {
			case gray:
				// Found a cycle: cut the stack at next.
				for i, n := range stack {
					if n == next {
						return append(append([]string(nil), stack[i:]...), next)
					}
				}
			case white:
				if cycle := dfs(next); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = black
		return nil
	}
	for name := range g.nodes {
		if state[name] == white {
			if cycle := dfs(name); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}
